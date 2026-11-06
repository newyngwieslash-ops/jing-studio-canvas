package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yingce/backend/internal/database"
	"yingce/backend/internal/model"
	"yingce/backend/internal/protocol"
	"yingce/backend/internal/repository"
	"yingce/backend/internal/storage"
)

func TestOpenAIImageLocalReferencesWithoutPublicURL(t *testing.T) {
	service, _, imageBytes := cloudAgentVisionFixture(t)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "")
	input := canvasGenerationInput{
		Mode: "image", Prompt: "Keep the character consistent with @图片1",
		Config:          providerConfig{InterfaceType: string(model.ChannelInterfaceOpenAIImage), Model: "test-image"},
		ReferenceImages: []providerMedia{{ID: "character", StorageKey: "resource:ref-one"}},
	}
	ctx := ensureOfficialProtocolAdapter(context.Background(), input.Config.InterfaceType)
	policy := providerMediaHydrationPolicyFor(ctx, input)
	if policy.requireURL {
		t.Fatal("local OpenAI image references must not require a public URL")
	}
	if err := service.hydrateGenerationMedia("user", &input, policy); err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	if input.ReferenceImages[0].DataURL != want || input.ReferenceImages[0].URL != "" {
		t.Fatal("reference did not hydrate from the authorized local resource")
	}
	adapter, ok := declarativeProtocolAdapterForContext(ctx, input.Config.InterfaceType)
	if !ok {
		t.Fatal("OpenAI image adapter is missing")
	}
	request, err := adapter.BuildCreate(ctx, protocol.RequestContext{Request: protocolRequestFromInput(input)})
	if err != nil {
		t.Fatal(err)
	}
	if request.Path != "/v1/images/edits" || request.ContentType != "multipart/form-data" || len(request.Files) != 1 || request.Files[0].Name != "image" || request.Files[0].Reference.DataURL != want {
		t.Fatal("image edit request did not retain inline image bytes as a multipart file")
	}
	foreign := canvasGenerationInput{ReferenceImages: []providerMedia{{StorageKey: "resource:ref-one"}}}
	if err := service.hydrateGenerationMedia("other-user", &foreign, policy); err == nil {
		t.Fatal("another user's reference resource was accepted")
	}
}

func TestVideoLocalReferencePromotesToActiveOSSWithoutPublicURL(t *testing.T) {
	db, err := database.Open(database.Config{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, dbErr := db.DB(); dbErr == nil {
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.SystemSetting{}, &model.UserOSSSetting{}, &model.StorageLocation{}, &model.Resource{}, &model.ResourceOffloadJob{}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	service := &Service{repo: repository.New(db), dataDir: dataDir}
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "")

	imageBytes := []byte("png-reference-bytes")
	if err := os.MkdirAll(filepath.Join(dataDir, "resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "resources", "ref-one.png"), imageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Resource{
		ID:        "ref-one",
		UserID:    "user",
		Kind:      "image",
		Status:    model.ResourceStatusReady,
		Provider:  "local",
		ObjectKey: "ref-one.png",
		MimeType:  "image/png",
		Size:      int64(len(imageBytes)),
	}).Error; err != nil {
		t.Fatal(err)
	}

	settingJSON, err := json.Marshal(ossSettingValue{
		Enabled:         true,
		Provider:        "qiniu",
		Region:          "z2",
		Endpoint:        "https://up-z2.qiniup.com",
		Bucket:          "yingce-dev",
		AccessKeyID:     "test-ak",
		AccessKeySecret: "test-sk",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.repo.SaveSystemSetting(&model.SystemSetting{Key: ossSettingKey, ValueJSON: string(settingJSON)}); err != nil {
		t.Fatal(err)
	}

	origPut := putOSSObjectContext
	t.Cleanup(func() { putOSSObjectContext = origPut })
	var uploadedKey string
	var uploadedBytes []byte
	putOSSObjectContext = func(_ context.Context, setting storage.Settings, objectKey string, mimeType string, size int64, body io.Reader) (string, error) {
		data, readErr := io.ReadAll(body)
		if readErr != nil {
			return "", readErr
		}
		uploadedKey = objectKey
		uploadedBytes = data
		return "etag-promoted", nil
	}

	input := canvasGenerationInput{
		Mode:            "video",
		Prompt:          "Generate video with @参考图1",
		Config:          providerConfig{InterfaceType: string(model.ChannelInterfaceNewAPIChannel1), Model: "wan3.0-video"},
		ReferenceImages: []providerMedia{{ID: "参考图1", StorageKey: "resource:ref-one"}},
	}
	ctx := ensureOfficialProtocolAdapter(context.Background(), input.Config.InterfaceType)
	policy := providerMediaHydrationPolicyFor(ctx, input)
	if !policy.requireURL {
		t.Fatal("newapi-channel-1 video references must require a URL")
	}
	if err := service.hydrateGenerationMedia("user", &input, policy); err != nil {
		t.Fatalf("hydrateGenerationMedia() error = %v", err)
	}
	if uploadedKey == "" || string(uploadedBytes) != string(imageBytes) {
		t.Fatalf("uploadedKey = %q, uploaded len = %d, want %d", uploadedKey, len(uploadedBytes), len(imageBytes))
	}
	if !strings.HasPrefix(input.ReferenceImages[0].URL, "https://yingce-dev.s3.cn-south-1.qiniucs.com/"+uploadedKey) {
		t.Fatalf("ReferenceImages[0].URL = %q", input.ReferenceImages[0].URL)
	}
	stored, err := service.repo.Resource("ref-one")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Provider != "qiniu" || stored.Bucket != "yingce-dev" || stored.ObjectKey != uploadedKey || stored.ETag != "etag-promoted" {
		t.Fatalf("promoted resource = %+v", stored)
	}
}
