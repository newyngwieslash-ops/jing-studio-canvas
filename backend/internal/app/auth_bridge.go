package app

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"yingce/backend/internal/auth"
	"yingce/backend/internal/model"
	"yingce/backend/internal/platform"
)

const SessionCookieName = auth.SessionCookieName

type (
	EmailCodeCooldownError     = auth.EmailCodeCooldownError
	RegisterRequest            = auth.RegisterRequest
	LoginRequest               = auth.LoginRequest
	LoginContext               = auth.LoginContext
	PublicAuthSettings         = auth.PublicAuthSettings
	AuthSessionResult          = auth.AuthSessionResult
	AuthUser                   = auth.AuthUser
	RegistrationSettingRequest = auth.RegistrationSettingRequest
	PublicRegistrationSetting  = auth.PublicRegistrationSetting
	PasswordResetRequest       = auth.PasswordResetRequest
	EmailSettingRequest        = auth.EmailSettingRequest
	PublicEmailSetting         = auth.PublicEmailSetting
	EmailLogQuery              = auth.EmailLogQuery
	EmailLogPage               = auth.EmailLogPage
	LinuxDOSettingRequest      = auth.LinuxDOSettingRequest
	PublicLinuxDOSetting       = auth.PublicLinuxDOSetting
	LinuxDOCallbackResult      = auth.OAuthCallbackResult
	LibTVSettingRequest        = auth.LibTVSettingRequest
	PublicLibTVSetting         = auth.PublicLibTVSetting
	LibTVImportRequest         = auth.LibTVImportRequest
	LibTVImportResult          = auth.LibTVImportResult
	LibTVCanvasConnection      = auth.LibTVCanvasConnection
	LibTVCanvasNode            = auth.LibTVCanvasNode
	LibTVImportIssue           = auth.LibTVImportIssue
	LibTVImportMetadata        = auth.LibTVImportMetadata
	LibTVImportWarning         = auth.LibTVImportWarning
	TapNowImportRequest        = auth.TapNowImportRequest
	TapNowImportResult         = auth.TapNowImportResult
	TapNowCanvasConnection     = auth.TapNowCanvasConnection
	TapNowCanvasNode           = auth.TapNowCanvasNode
	TapNowImportIssue          = auth.TapNowImportIssue
	TapNowImportMetadata       = auth.TapNowImportMetadata
	TapNowImportWarning        = auth.TapNowImportWarning
)

type authHost struct {
	svc *Service
}

func (h authHost) RequireAdmin(user *model.User) error {
	if h.svc == nil {
		return nil
	}
	return h.svc.RequireAdmin(user)
}

func (h authHost) EncryptSecret(value string) (string, error) {
	if h.svc == nil {
		return value, nil
	}
	return h.svc.encryptSettingSecret(value)
}

func (h authHost) DecryptSecret(value string) (string, error) {
	if h.svc == nil {
		return value, nil
	}
	return h.svc.decryptSettingSecret(value)
}

func (h authHost) SettingsEncryptionKey() ([]byte, error) {
	if h.svc == nil {
		return nil, nil
	}
	return h.svc.settingsEncryptionKey()
}

func (h authHost) BrandName() string {
	if h.svc == nil {
		return auth.DefaultBrandName
	}
	return h.svc.appearanceBrandName()
}

func (h authHost) EnsureSignupBonus(userID string) error {
	if h.svc == nil {
		return nil
	}
	return h.svc.ensureSignupBonus(userID)
}

func (h authHost) BindReferral(inviteeID string, code string) {
	if h.svc == nil {
		return
	}
	h.svc.bindReferral(inviteeID, code)
}
func (h authHost) RecordActivity(userID string, event string, count int) {
	if h.svc == nil {
		return
	}
	h.svc.recordActivity(userID, event, count)
}

func (h authHost) AllowRequest(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	if h.svc == nil {
		return true, nil
	}
	return h.svc.AllowRequest(ctx, key, limit, window)
}

func (h authHost) RequestRetryAfter(ctx context.Context, key string, window time.Duration) time.Duration {
	if h.svc == nil {
		return 0
	}
	return h.svc.RequestRetryAfter(ctx, key, window)
}

func (s *Service) authDomain() *auth.Service {
	if s == nil {
		return auth.New(nil, nil, nil)
	}
	if s.auth != nil {
		return s.auth
	}
	domain := auth.New(s.repo, authHost{svc: s}, nil)
	domain.SetSMSDelivery(s.smsDomain())
	return domain
}

func (s *Service) PublicAuthSettings() (*PublicAuthSettings, error) {
	return s.authDomain().PublicAuthSettings()
}

func (s *Service) Register(req RegisterRequest) (*AuthSessionResult, error) {
	return s.authDomain().Register(req)
}

func (s *Service) Login(req LoginRequest) (*AuthSessionResult, error) {
	return s.authDomain().Login(req)
}

func (s *Service) LoginRateLimitSubject(account string) (string, error) {
	return s.authDomain().LoginRateLimitSubject(account)
}

const (
	authSessionCacheTTL           = 3 * time.Second
	authRevocationTTL             = 10 * time.Second
	authRevocationReadTimeout     = 150 * time.Millisecond
	authRevocationWriteTimeout    = 250 * time.Millisecond
	authRevocationMaxLocalEntries = 8192
	authRevokedSessionRedisPrefix = "canvas:auth:revoked-session:"
	authRevokedUserRedisPrefix    = "canvas:auth:revoked-user:"
)

type cachedAuthSession struct {
	SessionID                   string
	User                        model.User
	SessionExpiresAt            time.Time
	LocalSeq                    uint64
	ObservedRedisUserRevocation string
}

type authUserRevocationEntry struct {
	seq       uint64
	token     string
	expiresAt time.Time
}

func parseAuthSessionCookie(value string) (string, string) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func authSessionCacheKey(sessionID, token string) string {
	return sessionID + ":" + hashToken(token)
}

func cloneAuthUserValue(user *model.User) model.User {
	if user == nil {
		return model.User{}
	}
	cloned := *user
	if user.EmailVerifiedAt != nil {
		t := *user.EmailVerifiedAt
		cloned.EmailVerifiedAt = &t
	}
	if user.PhoneVerifiedAt != nil {
		t := *user.PhoneVerifiedAt
		cloned.PhoneVerifiedAt = &t
	}
	if user.LastLoginAt != nil {
		t := *user.LastLoginAt
		cloned.LastLoginAt = &t
	}
	return cloned
}

func cloneAuthUser(user *model.User) *model.User {
	if user == nil {
		return nil
	}
	cloned := cloneAuthUserValue(user)
	return &cloned
}

func (s *Service) authRedis() *redis.Client {
	if s == nil || s.coordinator == nil {
		return nil
	}
	return s.coordinator.Redis()
}

func (s *Service) pruneAuthRevocationsLocked(now time.Time) {
	for sid, exp := range s.revokedSessions {
		if !exp.After(now) {
			delete(s.revokedSessions, sid)
		}
	}
	if excess := len(s.revokedSessions) - authRevocationMaxLocalEntries; excess == 1 {
		var oldestKey string
		var oldestExp time.Time
		for sid, exp := range s.revokedSessions {
			if oldestKey == "" || exp.Before(oldestExp) {
				oldestKey, oldestExp = sid, exp
			}
		}
		if oldestKey != "" {
			delete(s.revokedSessions, oldestKey)
		}
	} else if excess > 1 {
		keys := make([]string, 0, len(s.revokedSessions))
		for sid := range s.revokedSessions {
			keys = append(keys, sid)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if c := s.revokedSessions[a].Compare(s.revokedSessions[b]); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
		for _, sid := range keys[:excess] {
			delete(s.revokedSessions, sid)
		}
	}

	for uid, entry := range s.revokedUsersAt {
		if !entry.expiresAt.After(now) {
			delete(s.revokedUsersAt, uid)
		}
	}
	if excess := len(s.revokedUsersAt) - authRevocationMaxLocalEntries; excess == 1 {
		var oldestKey string
		var oldestEntry authUserRevocationEntry
		for uid, entry := range s.revokedUsersAt {
			if oldestKey == "" || entry.expiresAt.Before(oldestEntry.expiresAt) || (entry.expiresAt.Equal(oldestEntry.expiresAt) && entry.seq < oldestEntry.seq) {
				oldestKey, oldestEntry = uid, entry
			}
		}
		if oldestKey != "" {
			delete(s.revokedUsersAt, oldestKey)
		}
	} else if excess > 1 {
		keys := make([]string, 0, len(s.revokedUsersAt))
		for uid := range s.revokedUsersAt {
			keys = append(keys, uid)
		}
		slices.SortFunc(keys, func(a, b string) int {
			ea, eb := s.revokedUsersAt[a], s.revokedUsersAt[b]
			if c := ea.expiresAt.Compare(eb.expiresAt); c != 0 {
				return c
			}
			if c := cmp.Compare(ea.seq, eb.seq); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
		for _, uid := range keys[:excess] {
			delete(s.revokedUsersAt, uid)
		}
	}
}

func (s *Service) invalidateSessionAuthCache(sessionID, cacheKey string) {
	if s == nil || sessionID == "" {
		return
	}
	s.ensureReadCaches()
	if cacheKey != "" {
		s.authSessionCache.Delete(cacheKey)
	}
	now := time.Now()
	s.authRevocationMu.Lock()
	if s.revokedSessions == nil {
		s.revokedSessions = make(map[string]time.Time)
	}
	s.revokedSessions[sessionID] = now.Add(authRevocationTTL)
	s.pruneAuthRevocationsLocked(now)
	s.authRevocationMu.Unlock()

	if rdb := s.authRedis(); rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), authRevocationWriteTimeout)
		_ = rdb.Set(ctx, authRevokedSessionRedisPrefix+sessionID, "1", authRevocationTTL).Err()
		cancel()
	}
}

func (s *Service) invalidateUserAuthCache(userIDs ...string) {
	if s == nil || len(userIDs) == 0 {
		return
	}
	revSeq := s.authLocalSeq.Add(1)
	revToken := newID()
	now := time.Now()
	expiresAt := now.Add(authRevocationTTL)

	unique := make([]string, 0, len(userIDs))
	seen := make(map[string]struct{}, len(userIDs))
	s.authRevocationMu.Lock()
	if s.revokedUsersAt == nil {
		s.revokedUsersAt = make(map[string]authUserRevocationEntry)
	}
	for _, rawID := range userIDs {
		uid := strings.TrimSpace(rawID)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		unique = append(unique, uid)
		prev := s.revokedUsersAt[uid]
		if revSeq >= prev.seq {
			s.revokedUsersAt[uid] = authUserRevocationEntry{seq: revSeq, token: revToken, expiresAt: expiresAt}
		}
	}
	s.pruneAuthRevocationsLocked(now)
	s.authRevocationMu.Unlock()

	if len(unique) == 0 {
		return
	}
	if rdb := s.authRedis(); rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), authRevocationWriteTimeout)
		pipe := rdb.Pipeline()
		for _, uid := range unique {
			pipe.Set(ctx, authRevokedUserRedisPrefix+uid, revToken, authRevocationTTL)
		}
		_, _ = pipe.Exec(ctx)
		cancel()
	}
}

func (s *Service) checkAuthCacheEntryRevoked(entry cachedAuthSession) (revoked bool, observedUserToken string, redisFailed bool) {
	var localSessionRevoked bool
	var localUserRevoked bool
	var localUserToken string
	now := time.Now()
	s.authRevocationMu.Lock()
	if exp, ok := s.revokedSessions[entry.SessionID]; ok {
		if exp.After(now) {
			localSessionRevoked = true
		} else {
			delete(s.revokedSessions, entry.SessionID)
		}
	}
	if rev, ok := s.revokedUsersAt[entry.User.ID]; ok {
		if rev.expiresAt.After(now) {
			if rev.seq >= entry.LocalSeq {
				localUserRevoked = true
				localUserToken = rev.token
			}
		} else {
			delete(s.revokedUsersAt, entry.User.ID)
		}
	}
	s.authRevocationMu.Unlock()
	if localSessionRevoked {
		return true, "", false
	}

	rdb := s.authRedis()
	if rdb == nil {
		if localUserRevoked {
			return true, localUserToken, false
		}
		return false, "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), authRevocationReadTimeout)
	defer cancel()
	pipe := rdb.Pipeline()
	sessCmd := pipe.Get(ctx, authRevokedSessionRedisPrefix+entry.SessionID)
	userCmd := pipe.Get(ctx, authRevokedUserRedisPrefix+entry.User.ID)
	_, _ = pipe.Exec(ctx)

	if sessVal, err := sessCmd.Result(); err == nil && sessVal != "" {
		return true, "", false
	} else if err != nil && !errors.Is(err, redis.Nil) {
		if localUserRevoked {
			return true, "", false
		}
		return false, "", true
	}

	userVal, err := userCmd.Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		if localUserRevoked {
			return true, "", false
		}
		return false, "", true
	}
	userVal = strings.TrimSpace(userVal)
	if userVal != "" && userVal != entry.ObservedRedisUserRevocation {
		return true, userVal, false
	}
	if localUserRevoked {
		return true, userVal, false
	}
	return false, userVal, false
}

func (s *Service) uncachedCurrentUser(cookieValue string) (*model.User, error) {
	user, err := s.authDomain().CurrentUser(cookieValue)
	if err != nil {
		return nil, err
	}
	return cloneAuthUser(user), nil
}

func (s *Service) Logout(cookieValue string, contexts ...LoginContext) error {
	err := s.authDomain().Logout(cookieValue, contexts...)
	sessionID, token := parseAuthSessionCookie(cookieValue)
	if err == nil && sessionID != "" && token != "" {
		s.invalidateSessionAuthCache(sessionID, authSessionCacheKey(sessionID, token))
	}
	return err
}

func (s *Service) CurrentUser(cookieValue string) (*model.User, error) {
	if s == nil {
		return s.authDomain().CurrentUser(cookieValue)
	}
	sessionID, token := parseAuthSessionCookie(cookieValue)
	if sessionID == "" || token == "" {
		return s.authDomain().CurrentUser(cookieValue)
	}
	s.ensureReadCaches()
	cacheKey := authSessionCacheKey(sessionID, token)

	var expectedUserToken string
	for attempt := 0; attempt < 2; attempt++ {
		loadedFromDB := false
		expectedTokenForLoad := expectedUserToken
		entry, err := s.authSessionCache.Get(context.Background(), cacheKey, func(context.Context) (cachedAuthSession, int, error) {
			loadedFromDB = true
			localSeq := s.authLocalSeq.Add(1)
			user, expiresAt, loadErr := s.authDomain().CurrentUserSession(cookieValue)
			if loadErr != nil {
				return cachedAuthSession{}, 0, loadErr
			}
			cloned := cloneAuthUserValue(user)
			bytes := 512 + len(sessionID) + len(cloned.ID) + len(cloned.Username) + len(cloned.Email) + len(cloned.DisplayName) + len(expectedTokenForLoad)
			return cachedAuthSession{
				SessionID:                   sessionID,
				User:                        cloned,
				SessionExpiresAt:            expiresAt,
				LocalSeq:                    localSeq,
				ObservedRedisUserRevocation: expectedTokenForLoad,
			}, bytes, nil
		})
		if err != nil {
			if errors.Is(err, platform.ErrReadCacheBusy) {
				return s.uncachedCurrentUser(cookieValue)
			}
			// Bypass BoundedReadCache's 1s error cooldown so transient DB hiccups cannot lock out valid sessions.
			s.authSessionCache.Delete(cacheKey)
			return nil, err
		}
		if !time.Now().Before(entry.SessionExpiresAt) {
			s.authSessionCache.Delete(cacheKey)
			return s.uncachedCurrentUser(cookieValue)
		}
		revoked, observedUserToken, redisFailed := s.checkAuthCacheEntryRevoked(entry)
		if redisFailed {
			s.authSessionCache.Delete(cacheKey)
			if loadedFromDB {
				return cloneAuthUser(&entry.User), nil
			}
			return s.uncachedCurrentUser(cookieValue)
		}
		if !revoked {
			return cloneAuthUser(&entry.User), nil
		}
		s.authSessionCache.Delete(cacheKey)
		if observedUserToken != "" && attempt == 0 {
			expectedUserToken = observedUserToken
			continue
		}
		break
	}
	return s.uncachedCurrentUser(cookieValue)
}

func (s *Service) PublicAuthUser(user *model.User) (AuthUser, error) {
	return s.authDomain().PublicAuthUser(user)
}

func (s *Service) AdminRegistrationSetting(actor *model.User) (*PublicRegistrationSetting, error) {
	return s.authDomain().AdminRegistrationSetting(actor)
}

func (s *Service) UpdateRegistrationSetting(actor *model.User, req RegistrationSettingRequest) (*PublicRegistrationSetting, error) {
	return s.authDomain().UpdateRegistrationSetting(actor, req)
}

func (s *Service) RegistrationEnabled() (bool, error) {
	return s.authDomain().RegistrationEnabled()
}

func (s *Service) SendPasswordResetEmailCode(rawEmail string) error {
	return s.authDomain().SendPasswordResetEmailCode(rawEmail)
}

func (s *Service) ResetPassword(req PasswordResetRequest) error {
	if err := s.authDomain().ResetPassword(req); err != nil {
		return err
	}
	if s != nil && s.repo != nil {
		if email := normalizeEmail(req.Email); email != "" {
			if user, err := s.repo.UserByEmail(email); err == nil && user != nil {
				s.invalidateUserAuthCache(user.ID)
			}
		}
	}
	return nil
}

func (s *Service) AdminEmailSetting(actor *model.User) (*PublicEmailSetting, error) {
	return s.authDomain().AdminEmailSetting(actor)
}

func (s *Service) UpdateEmailSetting(actor *model.User, req EmailSettingRequest) (*PublicEmailSetting, error) {
	return s.authDomain().UpdateEmailSetting(actor, req)
}

func (s *Service) EmailEnabled() (bool, error) {
	return s.authDomain().EmailEnabled()
}

func (s *Service) SendRegistrationEmailCode(rawEmail string) error {
	return s.authDomain().SendRegistrationEmailCode(rawEmail)
}

func (s *Service) VerifyRegistrationEmailCode(email string, rawCode string) (*model.EmailVerificationCode, error) {
	return s.authDomain().VerifyRegistrationEmailCode(email, rawCode)
}

func (s *Service) AdminLinuxDOSetting(actor *model.User) (*PublicLinuxDOSetting, error) {
	return s.authDomain().AdminLinuxDOSetting(actor)
}

func (s *Service) UpdateLinuxDOSetting(actor *model.User, req LinuxDOSettingRequest) (*PublicLinuxDOSetting, error) {
	return s.authDomain().UpdateLinuxDOSetting(actor, req)
}

func (s *Service) LinuxDOEnabled() bool {
	return s.authDomain().LinuxDOEnabled()
}

func (s *Service) BeginLinuxDOLogin(nextPath string, acceptedTerms bool) (string, error) {
	return s.authDomain().BeginLinuxDOLogin(nextPath, acceptedTerms)
}

func (s *Service) CompleteLinuxDOLogin(stateValue string, code string, cookieValue string, contexts ...LoginContext) (*LinuxDOCallbackResult, error) {
	return s.authDomain().CompleteLinuxDOLogin(stateValue, code, cookieValue, contexts...)
}

func (s *Service) BeginOIDCLogin(ctx context.Context, next string, acceptedTerms bool) (string, string, error) {
	return s.authDomain().BeginOIDCLogin(ctx, next, acceptedTerms)
}

func (s *Service) CompleteOIDCLogin(ctx context.Context, state, code string, login LoginContext) (*auth.OAuthCallbackResult, error) {
	return s.authDomain().CompleteOIDCLogin(ctx, state, code, login)
}

func (s *Service) AdminLibTVSetting(actor *model.User) (*PublicLibTVSetting, error) {
	return s.authDomain().AdminLibTVSetting(actor)
}

func (s *Service) UpdateLibTVSetting(actor *model.User, req LibTVSettingRequest) (*PublicLibTVSetting, error) {
	return s.authDomain().UpdateLibTVSetting(actor, req)
}

func (s *Service) TestLibTV(actor *model.User, projectUUID string) error {
	return s.authDomain().TestLibTV(actor, projectUUID)
}

func (s *Service) ImportLibTV(userID, canvasProjectID, projectUUID string) (*LibTVImportResult, error) {
	return s.authDomain().ImportLibTV(userID, canvasProjectID, projectUUID)
}

func (s *Service) ImportTapNow(userID, canvasProjectID, shareID string) (*TapNowImportResult, error) {
	return s.authDomain().ImportTapNow(userID, canvasProjectID, shareID)
}

func hashPassword(password string) (string, error) {
	return auth.HashPassword(password)
}

func normalizeUsername(value string) string {
	return auth.NormalizeUsername(value)
}

func normalizeEmail(value string) string {
	return auth.NormalizeEmail(value)
}

func normalizeDisplayName(value string, fallback string) string {
	return auth.NormalizeDisplayName(value, fallback)
}

func validateUsername(value string) error {
	return auth.ValidateUsername(value)
}

func validatePassword(value string) error {
	return auth.ValidatePassword(value)
}

func validateEmail(value string) error {
	return auth.ValidateEmail(value)
}

func hashToken(token string) string {
	return auth.HashToken(token)
}

func randomToken() string {
	return auth.RandomToken()
}

func (s *Service) AdminEmailLogs(actor *model.User, query EmailLogQuery, page, pageSize int) (*EmailLogPage, error) {
	return s.authDomain().AdminEmailLogs(actor, query, page, pageSize)
}

func (s *Service) SendTestEmail(actor *model.User, recipient string) error {
	return s.authDomain().SendTestEmail(actor, recipient)
}
