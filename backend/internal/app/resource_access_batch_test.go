package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"yingce/backend/internal/assets"
	"yingce/backend/internal/auth"
	"yingce/backend/internal/database"
	"yingce/backend/internal/model"
	"yingce/backend/internal/platform"
	"yingce/backend/internal/repository"
	"yingce/backend/internal/storage"
)

func openPureGoSQLiteTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	sanitizedName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := database.Open(database.Config{
		Driver: "sqlite",
		DSN:    fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", sanitizedName, time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw sql db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func registerQueryCounter(t *testing.T, db *gorm.DB) *atomic.Int64 {
	t.Helper()
	var count atomic.Int64
	cbName := fmt.Sprintf("count_queries_%d", time.Now().UnixNano())
	if err := db.Callback().Query().After("gorm:query").Register(cbName, func(*gorm.DB) {
		count.Add(1)
	}); err != nil {
		t.Fatalf("register query counter: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove(cbName)
	})
	return &count
}

func TestResourceAccessBatchAvoidsNPlusOneQueries(t *testing.T) {
	db := openPureGoSQLiteTestDB(t)
	if err := db.AutoMigrate(&model.Resource{}, &model.SystemSetting{}, &model.StorageLocation{}, &model.UserOSSSetting{}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, ".settings-key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{repo: repository.New(db), dataDir: dataDir}

	ownedIDs := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("res-owned-%02d", i)
		ownedIDs = append(ownedIDs, id)
		if err := db.Create(&model.Resource{
			ID:        id,
			UserID:    "u1",
			Kind:      "image",
			Status:    model.ResourceStatusReady,
			Provider:  "local",
			ObjectKey: "u1/" + id + ".png",
			MimeType:  "image/png",
			Size:      1024,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.Resource{
		ID:        "res-other-user",
		UserID:    "u2",
		Kind:      "image",
		Status:    model.ResourceStatusReady,
		Provider:  "local",
		ObjectKey: "u2/other.png",
		MimeType:  "image/png",
		Size:      1024,
	}).Error; err != nil {
		t.Fatal(err)
	}

	requests := make([]ResourceAccessRequest, 0, 23)
	for i := 0; i < 10; i++ {
		requests = append(requests, ResourceAccessRequest{
			ResourceID:    ownedIDs[i],
			AccessOptions: ResourceAccessOptions{Purpose: assets.PurposeDisplay, Variant: assets.VariantOriginal},
		})
	}
	requests = append(requests,
		ResourceAccessRequest{
			ResourceID:    "res-missing",
			AccessOptions: ResourceAccessOptions{Purpose: assets.PurposeDisplay, Variant: assets.VariantOriginal},
		},
		ResourceAccessRequest{
			ResourceID:    "res-other-user",
			AccessOptions: ResourceAccessOptions{Purpose: assets.PurposeDisplay, Variant: assets.VariantOriginal},
		},
		ResourceAccessRequest{
			ResourceID:    ownedIDs[0],
			AccessOptions: ResourceAccessOptions{Purpose: assets.PurposeDisplay, Variant: assets.VariantOriginal},
		},
	)
	for i := 10; i < 20; i++ {
		requests = append(requests, ResourceAccessRequest{
			ResourceID:    ownedIDs[i],
			AccessOptions: ResourceAccessOptions{Purpose: assets.PurposeDisplay, Variant: assets.VariantOriginal},
		})
	}

	queries := registerQueryCounter(t, db)
	queries.Store(0)

	results, err := svc.ResourceAccessBatch("u1", requests)
	if err != nil {
		t.Fatalf("ResourceAccessBatch error: %v", err)
	}
	if got := queries.Load(); got > 4 {
		t.Fatalf("ResourceAccessBatch (local) executed %d SQL queries for 23 items, want <= 4", got)
	}
	if len(results) != len(requests) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(requests))
	}
	for i, req := range requests {
		res := results[i]
		if res.ResourceID != req.ResourceID {
			t.Fatalf("results[%d].ResourceID = %q, want %q", i, res.ResourceID, req.ResourceID)
		}
		if req.ResourceID == "res-missing" || req.ResourceID == "res-other-user" {
			if res.Access != nil || res.Error == nil || res.Error.Code != 404 {
				t.Fatalf("results[%d] (%s) = %#v, want 404 error", i, req.ResourceID, res)
			}
			continue
		}
		if res.Error != nil || res.Access == nil || res.Access.URL == "" {
			t.Fatalf("results[%d] (%s) = %#v, want valid access URL", i, req.ResourceID, res)
		}
	}

	// Verify OSS batch memoization as well (20 OSS resources -> <= 4 SQL queries).
	setting := ossSettingValue{
		Enabled:         true,
		Provider:        aliyunOSSProvider,
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "batch-test-bucket",
		CDNBaseURL:      "https://cdn.example.com",
		AccessKeyID:     "test-ak",
		AccessKeySecret: "test-sk",
		Delivery:        storage.DeliverySettings{CDNAuthMode: "public"},
	}
	encodedSetting, err := json.Marshal(setting)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.repo.SaveSystemSetting(&model.SystemSetting{Key: ossSettingKey, ValueJSON: string(encodedSetting)}); err != nil {
		t.Fatal(err)
	}
	ossRequests := make([]ResourceAccessRequest, 0, 20)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("res-oss-%02d", i)
		if err := db.Create(&model.Resource{
			ID:        id,
			UserID:    "u1",
			Kind:      "image",
			Status:    model.ResourceStatusReady,
			Provider:  aliyunOSSProvider,
			Endpoint:  setting.Endpoint,
			Bucket:    setting.Bucket,
			ObjectKey: "u1/" + id + ".png",
			MimeType:  "image/png",
			Size:      2048,
		}).Error; err != nil {
			t.Fatal(err)
		}
		ossRequests = append(ossRequests, ResourceAccessRequest{
			ResourceID:    id,
			AccessOptions: ResourceAccessOptions{Purpose: assets.PurposeDisplay, Variant: assets.VariantOriginal},
		})
	}
	queries.Store(0)
	ossResults, err := svc.ResourceAccessBatch("u1", ossRequests)
	if err != nil {
		t.Fatalf("ResourceAccessBatch (OSS) error: %v", err)
	}
	if got := queries.Load(); got > 4 {
		t.Fatalf("ResourceAccessBatch (OSS) executed %d SQL queries for 20 items, want <= 4", got)
	}
	for i, res := range ossResults {
		if res.Error != nil || res.Access == nil || !strings.HasPrefix(res.Access.URL, "https://cdn.example.com/") {
			t.Fatalf("ossResults[%d] = %#v, want valid CDN URL", i, res)
		}
	}
}

func TestUserAssetsPageCharacterCardsAvoidsNPlusOneQueries(t *testing.T) {
	db := openPureGoSQLiteTestDB(t)
	if err := db.AutoMigrate(
		&model.Asset{},
		&model.AssetVersion{},
		&model.AssetRepresentation{},
		&model.CharacterVoiceBinding{},
		&model.VoiceProfile{},
	); err != nil {
		t.Fatal(err)
	}
	svc := &Service{repo: repository.New(db), dataDir: t.TempDir()}

	now := time.Now().UTC().Truncate(time.Second)
	activeProfile := model.VoiceProfile{
		ID:                   "vp-active",
		UserID:               "u1",
		Name:                 "少年音",
		Provider:             "user_upload",
		VoiceKey:             "sample:res-voice-1",
		Language:             "zh",
		Timbre:               "clear",
		SampleResourceID:     "res-voice-1",
		CompatibleModelsJSON: `["tts-1"]`,
		Status:               "active",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	inactiveProfile := model.VoiceProfile{
		ID:                   "vp-inactive",
		UserID:               "u1",
		Name:                 "停用音色",
		Provider:             "system",
		VoiceKey:             "disabled-key",
		CompatibleModelsJSON: `[]`,
		Status:               "disabled",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := db.Create(&activeProfile).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&inactiveProfile).Error; err != nil {
		t.Fatal(err)
	}

	createdAssets := make([]model.Asset, 0, 15)
	for i := 0; i < 15; i++ {
		assetID := fmt.Sprintf("char-asset-%02d", i)
		versionID := fmt.Sprintf("char-ver-%02d", i)
		defJSON := fmt.Sprintf(`{"age":%d,"archetype":"hero-%d"}`, 18+i, i)
		payload, err := characterAssetPayload(assetID, versionID, fmt.Sprintf("角色-%02d", i), json.RawMessage(defJSON), now, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if i == 14 {
			// Malformed DefinitionJSON on version row: must fall back per-card without failing the page.
			defJSON = "{malformed-json"
		}
		asset := model.Asset{
			ID:               assetID,
			UserID:           "u1",
			Kind:             "entity",
			Category:         model.AssetCategoryCharacter,
			Status:           model.AssetVersionStatusConfirmed,
			PrimaryVersionID: versionID,
			Title:            fmt.Sprintf("角色-%02d", i),
			PayloadJSON:      payload,
			CreatedAt:        now,
			UpdatedAt:        now.Add(time.Duration(i) * time.Second),
		}
		version := model.AssetVersion{
			ID:             versionID,
			AssetID:        assetID,
			Version:        1 + (i % 3),
			Status:         model.AssetVersionStatusConfirmed,
			DefinitionJSON: defJSON,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		var reps []model.AssetRepresentation
		switch i % 4 {
		case 0:
			// Insert out of role order with distinct CreatedAt timestamps to lock role asc, created_at asc ordering.
			reps = []model.AssetRepresentation{
				{ID: fmt.Sprintf("rep-%02d-a", i), TaskID: fmt.Sprintf("task-%02d-a", i), AssetVersionID: versionID, ResourceID: fmt.Sprintf("img-turn-%02d", i), MediaType: "image", Role: "turnaround_sheet", MetadataJSON: `{}`, CreatedAt: now.Add(2 * time.Second)},
				{ID: fmt.Sprintf("rep-%02d-b", i), TaskID: fmt.Sprintf("task-%02d-b", i), AssetVersionID: versionID, ResourceID: fmt.Sprintf("img-prim-%02d", i), MediaType: "image", Role: "primary", MetadataJSON: `{}`, CreatedAt: now.Add(1 * time.Second)},
			}
		case 1:
			reps = []model.AssetRepresentation{
				{ID: fmt.Sprintf("rep-%02d-s", i), TaskID: fmt.Sprintf("task-%02d-s", i), AssetVersionID: versionID, ResourceID: fmt.Sprintf("img-side-%02d", i), MediaType: "image", Role: "side", MetadataJSON: `{}`, CreatedAt: now.Add(2 * time.Second)},
				{ID: fmt.Sprintf("rep-%02d-f", i), TaskID: fmt.Sprintf("task-%02d-f", i), AssetVersionID: versionID, ResourceID: fmt.Sprintf("img-front-%02d", i), MediaType: "image", Role: "front", MetadataJSON: `{}`, CreatedAt: now.Add(1 * time.Second)},
				{ID: fmt.Sprintf("rep-%02d-k", i), TaskID: fmt.Sprintf("task-%02d-k", i), AssetVersionID: versionID, ResourceID: fmt.Sprintf("img-back-%02d", i), MediaType: "image", Role: "back", MetadataJSON: `{}`, CreatedAt: now.Add(3 * time.Second)},
			}
		case 2:
			reps = []model.AssetRepresentation{
				{ID: fmt.Sprintf("rep-%02d-p", i), TaskID: fmt.Sprintf("task-%02d-p", i), AssetVersionID: versionID, ResourceID: fmt.Sprintf("img-partial-%02d", i), MediaType: "image", Role: "front", MetadataJSON: `{}`, CreatedAt: now.Add(1 * time.Second)},
			}
		case 3:
			// Missing representations -> visualStatus = "missing"
		}
		var voice *model.CharacterVoiceBinding
		switch i % 3 {
		case 0:
			voice = &model.CharacterVoiceBinding{
				ID:             fmt.Sprintf("vb-%02d", i),
				AssetVersionID: versionID,
				VoiceProfileID: activeProfile.ID,
				Instructions:   "calm",
				CreatedAt:      now,
				UpdatedAt:      now,
			}
		case 1:
			voice = &model.CharacterVoiceBinding{
				ID:             fmt.Sprintf("vb-%02d", i),
				AssetVersionID: versionID,
				VoiceProfileID: inactiveProfile.ID,
				Instructions:   "whisper",
				CreatedAt:      now,
				UpdatedAt:      now,
			}
		case 2:
			// No voice binding -> voiceStatus = "missing"
		}
		if err := svc.repo.CreateUserCharacter(&asset, &version, reps, voice); err != nil {
			t.Fatal(err)
		}
		createdAssets = append(createdAssets, asset)
	}

	// Capture reference single-card summaries & payloads BEFORE counting queries.
	singleCardsByID := make(map[string]CharacterCardSummary, len(createdAssets))
	expectedByID := make(map[string]map[string]any, len(createdAssets))
	for _, asset := range createdAssets {
		assetCopy := asset
		singleRaw := clientAssetListPayload(assetCopy)
		if singleCard, err := svc.characterCard("u1", &assetCopy); err == nil {
			singleCardsByID[assetCopy.ID] = singleCard
			singleRaw = encodeCharacterAssetListPayload(singleCard, singleRaw)
		}
		var parsed map[string]any
		if err := json.Unmarshal(singleRaw, &parsed); err != nil {
			t.Fatal(err)
		}
		expectedByID[assetCopy.ID] = parsed
	}
	batchCards, err := svc.characterCardsForAssets("u1", createdAssets)
	if err != nil {
		t.Fatalf("characterCardsForAssets error: %v", err)
	}
	if !reflect.DeepEqual(batchCards, singleCardsByID) {
		t.Fatalf("batch characterCardsForAssets mismatch vs single characterCard:\ngot:  %#v\nwant: %#v", batchCards, singleCardsByID)
	}

	queries := registerQueryCounter(t, db)
	queries.Store(0)

	page, err := svc.UserAssetsPage("u1", 1, 40, UserAssetPageFilter{})
	if err != nil {
		t.Fatalf("UserAssetsPage error: %v", err)
	}
	if got := queries.Load(); got > 8 {
		t.Fatalf("UserAssetsPage executed %d SQL queries for 15 characters, want <= 8", got)
	}
	if len(page.Assets) != 15 {
		t.Fatalf("len(page.Assets) = %d, want 15", len(page.Assets))
	}

	for _, raw := range page.Assets {
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		id, _ := got["id"].(string)
		want, ok := expectedByID[id]
		if !ok {
			t.Fatalf("unexpected asset id %q", id)
		}
		gotData, _ := got["data"].(map[string]any)
		wantData, _ := want["data"].(map[string]any)
		for _, field := range []string{"coverStorageKey", "voiceName", "voiceSampleStorageKey", "visualStatus", "voiceStatus", "version"} {
			if fmt.Sprint(gotData[field]) != fmt.Sprint(wantData[field]) {
				t.Fatalf("asset %s field %s = %v, want %v", id, field, gotData[field], wantData[field])
			}
		}
		gotDef, _ := json.Marshal(gotData["definition"])
		wantDef, _ := json.Marshal(wantData["definition"])
		if string(gotDef) != string(wantDef) {
			t.Fatalf("asset %s definition = %s, want %s", id, gotDef, wantDef)
		}
	}
}

func TestCurrentUserCacheHitShallowCopyAndCrossReplicaRevocation(t *testing.T) {
	db := openPureGoSQLiteTestDB(t)
	if err := db.AutoMigrate(
		&model.User{},
		&model.AuthSession{},
		&model.AdminAuditEvent{},
		&model.LoginLog{},
		&model.TaskTextDelta{},
		&model.EmailVerificationCode{},
		&model.SystemSetting{},
		&model.EmailLog{},
	); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, ".settings-key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{repo: repository.New(db), dataDir: dataDir}
	var deliveredCode string
	svc.auth = auth.New(svc.repo, authHost{svc: svc}, func(_ auth.EmailSettingValue, _ string, _ string, body string) error {
		marker := "验证码："
		if idx := strings.Index(body, marker); idx >= 0 {
			rest := body[idx+len(marker):]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				rest = rest[:nl]
			}
			deliveredCode = strings.TrimSpace(rest)
		}
		return nil
	})

	admin := &model.User{ID: "admin-1", Username: "admin", DisplayName: "Admin", Role: model.UserRoleAdmin, Status: model.UserStatusActive}
	pwHash, err := hashPassword("password-12345")
	if err != nil {
		t.Fatal(err)
	}
	loginAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	member := &model.User{
		ID:           "member-1",
		Username:     "member",
		Email:        "member@gmail.com",
		DisplayName:  "Member",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
		PasswordHash: pwHash,
		LastLoginAt:  &loginAt,
	}
	targetDelete := &model.User{ID: "delete-1", Username: "deluser", Role: model.UserRoleUser, Status: model.UserStatusActive}
	targetBulk := &model.User{ID: "bulk-1", Username: "bulkuser", Role: model.UserRoleUser, Status: model.UserStatusActive}
	targetReset := &model.User{ID: "reset-1", Username: "resetuser", Email: "reset@gmail.com", Role: model.UserRoleUser, Status: model.UserStatusActive, PasswordHash: pwHash}
	for _, u := range []*model.User{admin, member, targetDelete, targetBulk, targetReset} {
		if err := db.Create(u).Error; err != nil {
			t.Fatal(err)
		}
	}

	createSession := func(id, userID, secret string, ttl time.Duration) string {
		if err := db.Create(&model.AuthSession{
			ID:        id,
			UserID:    userID,
			TokenHash: hashToken(secret),
			ExpiresAt: time.Now().Add(ttl),
		}).Error; err != nil {
			t.Fatal(err)
		}
		return id + "." + secret
	}

	cookie1 := createSession("sess-1", member.ID, "secret-1", time.Hour)
	queries := registerQueryCounter(t, db)

	// 1. First read populates cache; second read within 3s executes 0 SQL queries and returns shallow/isolated copy.
	queries.Store(0)
	u1, err := svc.CurrentUser(cookie1)
	if err != nil {
		t.Fatalf("1st CurrentUser error: %v", err)
	}
	if queries.Load() == 0 {
		t.Fatal("expected 1st CurrentUser miss to query DB")
	}
	// Mutate returned struct & pointer field to ensure cached entry cannot be corrupted by callers.
	u1.Role = model.UserRoleAdmin
	u1.DisplayName = "MutatedInCaller"
	if u1.LastLoginAt != nil {
		*u1.LastLoginAt = time.Unix(0, 0)
	}

	queries.Store(0)
	u2, err := svc.CurrentUser(cookie1)
	if err != nil {
		t.Fatalf("2nd CurrentUser error: %v", err)
	}
	if got := queries.Load(); got != 0 {
		t.Fatalf("2nd CurrentUser within 3s executed %d SQL queries, want 0", got)
	}
	if u2.Role != model.UserRoleUser || u2.DisplayName != "Member" || u2.LastLoginAt == nil || !u2.LastLoginAt.Equal(loginAt) {
		t.Fatalf("cached user was mutated by caller: %+v", u2)
	}

	// 2. Session expiry clamping: cached session must never outlive session.ExpiresAt.
	shortCookie := createSession("sess-short", member.ID, "secret-short", 300*time.Millisecond)
	if _, err := svc.CurrentUser(shortCookie); err != nil {
		t.Fatalf("short session initial read failed: %v", err)
	}
	time.Sleep(360 * time.Millisecond)
	if _, err := svc.CurrentUser(shortCookie); err == nil {
		t.Fatal("expired session was served from 3s cache past session.ExpiresAt")
	}

	// 3. Local 0ms invalidation: UpdateUser Role change (user -> admin) reflects immediately without deleting session.
	if _, err := svc.UpdateUser(admin, member.ID, UpdateUserRequest{Role: model.UserRoleAdmin}); err != nil {
		t.Fatal(err)
	}
	uAdmin, err := svc.CurrentUser(cookie1)
	if err != nil {
		t.Fatalf("CurrentUser after role upgrade failed: %v", err)
	}
	if uAdmin.Role != model.UserRoleAdmin {
		t.Fatalf("CurrentUser after role upgrade returned role=%q, want admin", uAdmin.Role)
	}
	if _, err := svc.UpdateUser(admin, member.ID, UpdateUserRequest{Role: model.UserRoleUser}); err != nil {
		t.Fatal(err)
	}

	// 4. Local 0ms invalidation: Logout.
	if _, err := svc.CurrentUser(cookie1); err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(cookie1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CurrentUser(cookie1); err == nil {
		t.Fatal("CurrentUser succeeded after Logout")
	}

	// 5. Local 0ms invalidation: UpdateUser(Disabled), plus re-enable + brand-new session immediately works.
	cookie2 := createSession("sess-2", member.ID, "secret-2", time.Hour)
	if _, err := svc.CurrentUser(cookie2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateUser(admin, member.ID, UpdateUserRequest{Status: model.UserStatusDisabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CurrentUser(cookie2); err == nil {
		t.Fatal("CurrentUser succeeded after UpdateUser(Disabled)")
	}
	if _, err := svc.UpdateUser(admin, member.ID, UpdateUserRequest{Status: model.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	cookieFresh := createSession("sess-fresh", member.ID, "secret-fresh", time.Hour)
	if _, err := svc.CurrentUser(cookieFresh); err != nil {
		t.Fatalf("fresh session after re-enable failed: %v", err)
	}

	// 6. Local 0ms invalidation: DeleteUser, BulkDisableUsers, ResetPassword.
	cookieDel := createSession("sess-del", targetDelete.ID, "secret-del", time.Hour)
	if _, err := svc.CurrentUser(cookieDel); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser(admin, targetDelete.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CurrentUser(cookieDel); err == nil {
		t.Fatal("CurrentUser succeeded after DeleteUser")
	}

	cookieBulk := createSession("sess-bulk", targetBulk.ID, "secret-bulk", time.Hour)
	if _, err := svc.CurrentUser(cookieBulk); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BulkDisableUsers(admin, BulkDisableUsersRequest{UserIDs: []string{targetBulk.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CurrentUser(cookieBulk); err == nil {
		t.Fatal("CurrentUser succeeded after BulkDisableUsers")
	}

	if _, err := svc.UpdateEmailSetting(admin, EmailSettingRequest{
		Enabled:    true,
		Host:       "smtp.example.com",
		Port:       587,
		Encryption: "starttls",
		FromEmail:  "noreply@example.com",
		FromName:   "YingCe",
	}); err != nil {
		t.Fatal(err)
	}
	cookieReset := createSession("sess-reset", targetReset.ID, "secret-reset", time.Hour)
	if _, err := svc.CurrentUser(cookieReset); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendPasswordResetEmailCode(targetReset.Email); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetPassword(PasswordResetRequest{
		Email:     targetReset.Email,
		EmailCode: deliveredCode,
		Password:  "new-password-67890",
	}); err != nil {
		t.Fatalf("ResetPassword failed: %v", err)
	}
	if _, err := svc.CurrentUser(cookieReset); err == nil {
		t.Fatal("CurrentUser succeeded after ResetPassword")
	}

	// 7. Multi-replica 0ms cross-replica revocation over real TCP RESP2 + *redis.Client.
	redisAddr := startMiniRESP2KVServer(t)
	rdbA := redis.NewClient(&redis.Options{Addr: redisAddr, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdbA.Close() })
	rdbB := redis.NewClient(&redis.Options{Addr: redisAddr, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdbB.Close() })

	svcA := &Service{
		repo:        repository.New(db),
		dataDir:     dataDir,
		coordinator: platform.NewCoordinatorWithRedis(rdbA, "replica-a"),
	}
	svcB := &Service{
		repo:        repository.New(db),
		dataDir:     dataDir,
		coordinator: platform.NewCoordinatorWithRedis(rdbB, "replica-b"),
	}

	haUser1 := &model.User{ID: "ha-user-1", Username: "hauser1", DisplayName: "HA1", Role: model.UserRoleUser, Status: model.UserStatusActive}
	haUser2 := &model.User{ID: "ha-user-2", Username: "hauser2", DisplayName: "HA2", Role: model.UserRoleUser, Status: model.UserStatusActive}
	for _, u := range []*model.User{haUser1, haUser2} {
		if err := db.Create(u).Error; err != nil {
			t.Fatal(err)
		}
	}
	haCookie1 := createSession("ha-sess-1", haUser1.ID, "ha-sec-1", time.Hour)
	haCookieLogout := createSession("ha-sess-logout", haUser1.ID, "ha-sec-logout", time.Hour)
	haCookie2 := createSession("ha-sess-2", haUser2.ID, "ha-sec-2", time.Hour)

	// Warm cache on Replica B and verify 0 SQL on hit.
	if _, err := svcB.CurrentUser(haCookie1); err != nil {
		t.Fatal(err)
	}
	queries.Store(0)
	if _, err := svcB.CurrentUser(haCookie1); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 0 {
		t.Fatalf("Replica B cached CurrentUser executed %d SQL queries, want 0", got)
	}

	// Replica A upgrades haUser1 role -> Replica B immediately sees admin, then caches new epoch with 0 SQL!
	if _, err := svcA.UpdateUser(admin, haUser1.ID, UpdateUserRequest{Role: model.UserRoleAdmin}); err != nil {
		t.Fatal(err)
	}
	haGotRole, err := svcB.CurrentUser(haCookie1)
	if err != nil {
		t.Fatalf("Replica B CurrentUser after Replica A role update failed: %v", err)
	}
	if haGotRole.Role != model.UserRoleAdmin {
		t.Fatalf("Replica B saw stale role %q after Replica A UpdateUser, want admin", haGotRole.Role)
	}
	queries.Store(0)
	if _, err := svcB.CurrentUser(haCookie1); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 0 {
		t.Fatalf("Replica B post-refresh CurrentUser executed %d SQL queries, want 0", got)
	}

	// Replica B warms haCookieLogout -> Replica A runs Logout -> Replica B immediately rejects!
	if _, err := svcB.CurrentUser(haCookieLogout); err != nil {
		t.Fatal(err)
	}
	if err := svcA.Logout(haCookieLogout); err != nil {
		t.Fatal(err)
	}
	if _, err := svcB.CurrentUser(haCookieLogout); err == nil {
		t.Fatal("Replica B served cached session after Replica A Logout")
	}

	// Replica B still has haCookie1 cached -> Replica A disables haUser1 -> Replica B immediately rejects!
	if _, err := svcB.CurrentUser(haCookie1); err != nil {
		t.Fatal(err)
	}
	if _, err := svcA.UpdateUser(admin, haUser1.ID, UpdateUserRequest{Role: model.UserRoleUser, Status: model.UserStatusDisabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svcB.CurrentUser(haCookie1); err == nil {
		t.Fatal("Replica B served cached disabled user after Replica A UpdateUser(Disabled)")
	}

	// Disable -> immediate Re-enable -> brand-new session within 10s Redis revocation TTL:
	// Replica B must accept the new session AND cache it so repeat reads within 3s execute 0 SQL queries!
	if _, err := svcA.UpdateUser(admin, haUser1.ID, UpdateUserRequest{Status: model.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	haCookieReenabled := createSession("ha-sess-reenabled", haUser1.ID, "ha-sec-reenabled", time.Hour)
	if _, err := svcB.CurrentUser(haCookieReenabled); err != nil {
		t.Fatalf("Replica B rejected fresh session after re-enable within revocation TTL: %v", err)
	}
	queries.Store(0)
	if _, err := svcB.CurrentUser(haCookieReenabled); err != nil {
		t.Fatalf("Replica B 2nd CurrentUser on re-enabled session failed: %v", err)
	}
	if got := queries.Load(); got != 0 {
		t.Fatalf("Replica B 2nd CurrentUser on re-enabled session within 10s Redis TTL executed %d SQL queries, want 0", got)
	}

	// Clock-skew / arbitrary opaque token regression: even if another replica writes a numerically/lexicographically
	// smaller token ("1" or "0000-opaque-token") to canvas:auth:revoked-user:<uid>, Replica B must evict in 0ms,
	// reload the updated state, and then serve subsequent hits within 3s with 0 SQL queries!
	for _, tok := range []string{"1", "0000-opaque-skew-token"} {
		wantName := "SkewName-" + tok
		if err := db.Model(&model.User{}).Where("id = ?", haUser1.ID).Update("display_name", wantName).Error; err != nil {
			t.Fatal(err)
		}
		if err := rdbA.Set(context.Background(), authRevokedUserRedisPrefix+haUser1.ID, tok, 10*time.Second).Err(); err != nil {
			t.Fatal(err)
		}
		gotSkew, err := svcB.CurrentUser(haCookieReenabled)
		if err != nil {
			t.Fatalf("Replica B CurrentUser after opaque revocation token %q failed: %v", tok, err)
		}
		if gotSkew.DisplayName != wantName {
			t.Fatalf("Replica B ignored clock-skewed/opaque revocation token %q: got DisplayName=%q, want %q", tok, gotSkew.DisplayName, wantName)
		}
		queries.Store(0)
		if _, err := svcB.CurrentUser(haCookieReenabled); err != nil {
			t.Fatal(err)
		}
		if got := queries.Load(); got != 0 {
			t.Fatalf("Replica B post-opaque-token CurrentUser(%q) executed %d SQL queries, want 0", tok, got)
		}
	}

	// In-flight DB load race regression: if UpdateUser(Disabled) commits while CurrentUserSession is mid-flight
	// (loadedFromDB == true), CurrentUser must still check revocation, evict the stale entry, and reject!
	raceUser := &model.User{ID: "ha-user-race", Username: "hauserrace", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.Create(raceUser).Error; err != nil {
		t.Fatal(err)
	}
	raceCookie := createSession("ha-sess-race", raceUser.ID, "ha-sec-race", time.Hour)
	raceCbName := fmt.Sprintf("race_disable_%d", time.Now().UnixNano())
	var raceTriggered atomic.Bool
	if err := db.Callback().Query().After("gorm:query").Register(raceCbName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "users" && !raceTriggered.Load() {
			if u, ok := tx.Statement.Dest.(*model.User); ok && u.ID == raceUser.ID {
				if raceTriggered.CompareAndSwap(false, true) {
					_, _ = svcA.UpdateUser(admin, raceUser.ID, UpdateUserRequest{Status: model.UserStatusDisabled})
				}
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, raceErr := svcB.CurrentUser(raceCookie)
	_ = db.Callback().Query().Remove(raceCbName)
	if !raceTriggered.Load() {
		t.Fatal("in-flight race callback did not trigger")
	}
	if raceErr == nil {
		t.Fatal("in-flight CurrentUser returned stale active user despite concurrent UpdateUser(Disabled)")
	}
	if _, err := svcB.CurrentUser(raceCookie); err == nil {
		t.Fatal("subsequent CurrentUser served stale active user cached by racing load")
	}

	// Local-only (no Redis) in-flight DB load race regression:
	localRaceUser := &model.User{ID: "local-user-race", Username: "localuserrace", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.Create(localRaceUser).Error; err != nil {
		t.Fatal(err)
	}
	localRaceCookie := createSession("local-sess-race", localRaceUser.ID, "local-sec-race", time.Hour)
	localRaceCbName := fmt.Sprintf("local_race_disable_%d", time.Now().UnixNano())
	var localRaceTriggered atomic.Bool
	if err := db.Callback().Query().After("gorm:query").Register(localRaceCbName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "users" && !localRaceTriggered.Load() {
			if u, ok := tx.Statement.Dest.(*model.User); ok && u.ID == localRaceUser.ID {
				if localRaceTriggered.CompareAndSwap(false, true) {
					_, _ = svc.UpdateUser(admin, localRaceUser.ID, UpdateUserRequest{Status: model.UserStatusDisabled})
				}
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, localRaceErr := svc.CurrentUser(localRaceCookie)
	_ = db.Callback().Query().Remove(localRaceCbName)
	if !localRaceTriggered.Load() {
		t.Fatal("local in-flight race callback did not trigger")
	}
	if localRaceErr == nil {
		t.Fatal("local in-flight CurrentUser returned stale active user despite concurrent UpdateUser(Disabled)")
	}

	// Verify local revocation map cap holds across multi-ID batch insertion.
	svc.authRevocationMu.Lock()
	for i := 0; i < authRevocationMaxLocalEntries; i++ {
		svc.revokedUsersAt[fmt.Sprintf("cap-seed-%d", i)] = authUserRevocationEntry{
			seq:       uint64(i + 1),
			token:     "seed",
			expiresAt: time.Now().Add(authRevocationTTL),
		}
	}
	svc.authRevocationMu.Unlock()
	batchEvictIDs := make([]string, 25)
	for i := range batchEvictIDs {
		batchEvictIDs[i] = fmt.Sprintf("cap-batch-%d", i)
	}
	svc.invalidateUserAuthCache(batchEvictIDs...)
	svc.authRevocationMu.Lock()
	gotRevokedUsersLen := len(svc.revokedUsersAt)
	svc.authRevocationMu.Unlock()
	if gotRevokedUsersLen > authRevocationMaxLocalEntries {
		t.Fatalf("len(svc.revokedUsersAt) = %d after multi-ID batch, want <= %d", gotRevokedUsersLen, authRevocationMaxLocalEntries)
	}

	// Replica B warms haCookie2 -> Replica A runs BulkDisableUsers -> Replica B immediately rejects!
	if _, err := svcB.CurrentUser(haCookie2); err != nil {
		t.Fatal(err)
	}
	if _, err := svcA.BulkDisableUsers(admin, BulkDisableUsersRequest{UserIDs: []string{haUser2.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svcB.CurrentUser(haCookie2); err == nil {
		t.Fatal("Replica B served cached session after Replica A BulkDisableUsers")
	}

	// Fail-safe check when Redis client errors: active user is not locked out, and disabled user is not served from stale cache.
	haUser3 := &model.User{ID: "ha-user-3", Username: "hauser3", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := db.Create(haUser3).Error; err != nil {
		t.Fatal(err)
	}
	haCookie3 := createSession("ha-sess-3", haUser3.ID, "ha-sec-3", time.Hour)
	if _, err := svcB.CurrentUser(haCookie3); err != nil {
		t.Fatal(err)
	}
	_ = rdbB.Close()
	if _, err := svcB.CurrentUser(haCookie3); err != nil {
		t.Fatalf("Redis outage locked out active user on Replica B: %v", err)
	}
	if _, err := svcA.UpdateUser(admin, haUser3.ID, UpdateUserRequest{Status: model.UserStatusDisabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svcB.CurrentUser(haCookie3); err == nil {
		t.Fatal("Redis outage on Replica B allowed disabled user through stale cache")
	}
}

type miniRESP2KVEntry struct {
	value     string
	expiresAt time.Time
}

func startMiniRESP2KVServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mini RESP2 KV server: %v", err)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	closed := false
	store := map[string]miniRESP2KVEntry{}
	conns := map[net.Conn]struct{}{}

	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		closed = true
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			conns[conn] = struct{}{}
			wg.Add(1)
			mu.Unlock()
			go func(c net.Conn) {
				defer wg.Done()
				defer func() {
					_ = c.Close()
					mu.Lock()
					delete(conns, c)
					mu.Unlock()
				}()
				reader := bufio.NewReader(c)
				for {
					args, err := readRESP2Command(reader)
					if err != nil || len(args) == 0 {
						return
					}
					switch strings.ToUpper(args[0]) {
					case "HELLO", "CLIENT":
						_, _ = io.WriteString(c, "-ERR unknown command\r\n")
					case "PING":
						_, _ = io.WriteString(c, "+PONG\r\n")
					case "SET":
						if len(args) < 3 {
							_, _ = io.WriteString(c, "-ERR wrong number of arguments\r\n")
							continue
						}
						var expiresAt time.Time
						for i := 3; i+1 < len(args); i += 2 {
							opt := strings.ToUpper(args[i])
							n, parseErr := strconv.ParseInt(args[i+1], 10, 64)
							if parseErr != nil {
								continue
							}
							if opt == "EX" {
								expiresAt = time.Now().Add(time.Duration(n) * time.Second)
							} else if opt == "PX" {
								expiresAt = time.Now().Add(time.Duration(n) * time.Millisecond)
							}
						}
						mu.Lock()
						store[args[1]] = miniRESP2KVEntry{value: args[2], expiresAt: expiresAt}
						mu.Unlock()
						_, _ = io.WriteString(c, "+OK\r\n")
					case "GET":
						if len(args) < 2 {
							_, _ = io.WriteString(c, "-ERR wrong number of arguments\r\n")
							continue
						}
						now := time.Now()
						mu.Lock()
						entry, ok := store[args[1]]
						if ok && !entry.expiresAt.IsZero() && !entry.expiresAt.After(now) {
							delete(store, args[1])
							ok = false
						}
						mu.Unlock()
						if !ok {
							_, _ = io.WriteString(c, "$-1\r\n")
						} else {
							_, _ = io.WriteString(c, fmt.Sprintf("$%d\r\n%s\r\n", len(entry.value), entry.value))
						}
					case "DEL":
						removed := 0
						mu.Lock()
						for _, k := range args[1:] {
							if _, ok := store[k]; ok {
								delete(store, k)
								removed++
							}
						}
						mu.Unlock()
						_, _ = io.WriteString(c, fmt.Sprintf(":%d\r\n", removed))
					default:
						_, _ = io.WriteString(c, "+OK\r\n")
					}
				}
			}(conn)
		}
	}()

	return ln.Addr().String()
}
