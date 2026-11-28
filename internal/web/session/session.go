package session

import (
	"encoding/gob"
	"net/http"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
	"github.com/Dark-Sky07/phoenix/internal/util/random"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

const (
	loginUserKey       = "LOGIN_USER"
	loginEpochKey      = "LOGIN_EPOCH"
	sessionIDKey       = "SESSION_ID"
	recentAuthKey      = "RECENT_AUTH_AT"
	loginResellerKey   = "LOGIN_RESELLER"
	resellerEpochKey   = "LOGIN_RESELLER_EPOCH"
	apiAuthUserKey     = "api_auth_user"
	requestResellerKey = "authenticated_reseller"
	sessionCookieName  = "phoenix"
	trackedSessionTTL  = 30 * 24 * time.Hour
	trackedTouchEvery  = 5 * time.Minute
	reauthTTL          = 10 * time.Minute
)

func init() {
	gob.Register(model.User{})
	gob.Register(model.Reseller{})
}

// SetLoginReseller stores a reseller (نمایندگی) login in the session. Admin and
// reseller logins are mutually exclusive: signing in as one clears the other.
func SetLoginReseller(c *gin.Context, reseller *model.Reseller) error {
	if reseller == nil {
		return nil
	}
	s := sessions.Default(c)
	removeTrackedSession(stringValue(s.Get(sessionIDKey)))
	s.Delete(loginUserKey)
	s.Delete(loginEpochKey)
	s.Delete(sessionIDKey)
	s.Delete(recentAuthKey)
	s.Set(loginResellerKey, reseller.Id)
	s.Set(resellerEpochKey, reseller.LoginEpoch)
	return s.Save()
}

// GetLoginReseller returns the reseller bound to the current session, or nil
// when the session belongs to the admin (or nobody).
func GetLoginReseller(c *gin.Context) *model.Reseller {
	if reseller, ok := c.Get(requestResellerKey); ok {
		return reseller.(*model.Reseller)
	}
	return GetCurrentLoginReseller(c)
}

// PinLoginReseller keeps the authenticated role stable for this request. A
// revoked or temporarily unreadable session must not be mistaken for an admin.
func PinLoginReseller(c *gin.Context, reseller *model.Reseller) {
	if reseller != nil {
		c.Set(requestResellerKey, reseller)
	}
}

// GetCurrentLoginReseller revalidates the backing session and database rather
// than returning the request-scoped principal. Callers can reject a revoked
// session after waiting for a lock without changing the request's role.
func GetCurrentLoginReseller(c *gin.Context) *model.Reseller {
	if apiUser, ok := c.Get(apiAuthUserKey); ok {
		if _, isUser := apiUser.(*model.User); isUser {
			return nil
		}
	}
	s := sessions.Default(c)
	id := intValue(s.Get(loginResellerKey))
	if id <= 0 {
		return nil
	}
	reseller, err := getResellerByID(id)
	if err != nil {
		s.Delete(loginResellerKey)
		s.Delete(resellerEpochKey)
		if saveErr := s.Save(); saveErr != nil {
			logger.Warning("session: failed to drop missing reseller:", saveErr)
		}
		return nil
	}
	if int64Value(s.Get(resellerEpochKey)) != reseller.LoginEpoch {
		s.Delete(loginResellerKey)
		s.Delete(resellerEpochKey)
		if saveErr := s.Save(); saveErr != nil {
			logger.Warning("session: failed to drop stale reseller session:", saveErr)
		}
		return nil
	}
	return reseller
}

// IsResellerLogin reports whether the current session is a reseller session.
func IsResellerLogin(c *gin.Context) bool {
	return GetLoginReseller(c) != nil
}

// SetLoginUser records a new individually-revocable browser session. Cookies
// issued before this feature deliberately have no SESSION_ID and continue to
// work through their normal lifetime; this prevents an upgrade from logging
// out a panel's current operators.
func SetLoginUser(c *gin.Context, user *model.User) error {
	if user == nil {
		return nil
	}
	s := sessions.Default(c)
	removeTrackedSession(stringValue(s.Get(sessionIDKey)))
	trackedID, err := createTrackedSession(c, user)
	if err != nil {
		return err
	}
	s.Set(loginUserKey, user.Id)
	s.Set(loginEpochKey, user.LoginEpoch)
	if trackedID != "" {
		s.Set(sessionIDKey, trackedID)
	}
	SetRecentAuthentication(c)
	return s.Save()
}

func SetAPIAuthUser(c *gin.Context, user *model.User) {
	if user == nil {
		return
	}
	c.Set(apiAuthUserKey, user)
}

func GetLoginUser(c *gin.Context) *model.User {
	if v, ok := c.Get(apiAuthUserKey); ok {
		if u, ok2 := v.(*model.User); ok2 {
			return u
		}
	}
	s := sessions.Default(c)
	obj := s.Get(loginUserKey)
	if obj == nil {
		return nil
	}
	userID, ok := sessionUserID(obj)
	if !ok {
		s.Delete(loginUserKey)
		s.Delete(loginEpochKey)
		if err := s.Save(); err != nil {
			logger.Warning("session: failed to drop stale user payload:", err)
		}
		return nil
	}
	if legacyUserID, ok := legacySessionUserID(obj); ok {
		s.Set(loginUserKey, legacyUserID)
		if err := s.Save(); err != nil {
			logger.Warning("session: failed to migrate legacy user payload:", err)
		}
	}
	user, err := getUserByID(userID)
	if err != nil {
		logger.Warning("session: failed to load user:", err)
		s.Delete(loginUserKey)
		s.Delete(loginEpochKey)
		if saveErr := s.Save(); saveErr != nil {
			logger.Warning("session: failed to drop missing user:", saveErr)
		}
		return nil
	}
	if user.Disabled && user.Id != 1 {
		s.Delete(loginUserKey)
		s.Delete(loginEpochKey)
		_ = s.Save()
		return nil
	}
	if !sessionEpochMatches(s.Get(loginEpochKey), user.LoginEpoch) {
		s.Delete(loginUserKey)
		s.Delete(loginEpochKey)
		s.Delete(sessionIDKey)
		if saveErr := s.Save(); saveErr != nil {
			logger.Warning("session: failed to drop stale epoch:", saveErr)
		}
		return nil
	}
	// No session ID means a pre-upgrade cookie. Do not turn a security
	// migration into an involuntary logout. Every subsequently created login
	// receives a row and is checked/revocable below.
	if trackedID := stringValue(s.Get(sessionIDKey)); trackedID != "" {
		if !trackedSessionValid(c, trackedID, user) {
			s.Delete(loginUserKey)
			s.Delete(loginEpochKey)
			s.Delete(sessionIDKey)
			s.Delete(recentAuthKey)
			_ = s.Save()
			return nil
		}
	}
	return user
}

func sessionEpochMatches(cookieVal any, userEpoch int64) bool {
	var got int64
	switch v := cookieVal.(type) {
	case nil:
	case int64:
		got = v
	case int:
		got = int64(v)
	case int32:
		got = int64(v)
	case float64:
		got = int64(v)
	default:
		return false
	}
	return got == userEpoch
}

func IsLogin(c *gin.Context) bool {
	return GetLoginUser(c) != nil || GetLoginReseller(c) != nil
}

// intValue coerces a session-stored numeric value (gob round-trips ints as int)
// into an int, returning 0 when it is not a positive integer.
func intValue(obj any) int {
	switch v := obj.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case int32:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func int64Value(obj any) int64 {
	switch v := obj.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func getResellerByID(id int) (*model.Reseller, error) {
	db := database.GetDB()
	if db == nil {
		return nil, http.ErrServerClosed
	}
	reseller := &model.Reseller{}
	if err := db.Model(model.Reseller{}).Where("id = ?", id).First(reseller).Error; err != nil {
		return nil, err
	}
	return reseller, nil
}

func sessionUserID(obj any) (int, bool) {
	switch v := obj.(type) {
	case int:
		return v, v > 0
	case int64:
		return int(v), v > 0
	case int32:
		return int(v), v > 0
	case float64:
		id := int(v)
		return id, v == float64(id) && id > 0
	case model.User:
		return v.Id, v.Id > 0
	case *model.User:
		if v == nil {
			return 0, false
		}
		return v.Id, v.Id > 0
	default:
		return 0, false
	}
}

func legacySessionUserID(obj any) (int, bool) {
	switch v := obj.(type) {
	case model.User:
		return v.Id, v.Id > 0
	case *model.User:
		if v == nil {
			return 0, false
		}
		return v.Id, v.Id > 0
	default:
		return 0, false
	}
}

func getUserByID(id int) (*model.User, error) {
	db := database.GetDB()
	if db == nil {
		return nil, http.ErrServerClosed
	}
	user := &model.User{}
	if err := db.Model(model.User{}).Where("id = ?", id).First(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}

func ClearSession(c *gin.Context) error {
	s := sessions.Default(c)
	removeTrackedSession(stringValue(s.Get(sessionIDKey)))
	s.Clear()
	cookiePath := c.GetString("base_path")
	if cookiePath == "" {
		cookiePath = "/"
	}
	secure := c.Request.TLS != nil
	s.Options(sessions.Options{
		Path:     cookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	if err := s.Save(); err != nil {
		return err
	}
	if cookiePath != "/" {
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     sessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(0, 0),
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
	return nil
}

func stringValue(obj any) string {
	v, _ := obj.(string)
	return v
}

func createTrackedSession(c *gin.Context, user *model.User) (string, error) {
	db := database.GetDB()
	if db == nil {
		return "", nil
	}
	id := random.Seq(48)
	now := time.Now().UnixMilli()
	ua := c.Request.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	row := &model.UserSession{
		UserID:      user.Id,
		SessionHash: crypto.HashTokenSHA256(id),
		LoginEpoch:  user.LoginEpoch,
		LastSeenAt:  now,
		ExpiresAt:   time.Now().Add(trackedSessionTTL).UnixMilli(),
		IP:          c.ClientIP(),
		UserAgent:   ua,
	}
	if err := db.Create(row).Error; err != nil {
		return "", err
	}
	return id, nil
}

func removeTrackedSession(rawID string) {
	if rawID == "" {
		return
	}
	if db := database.GetDB(); db != nil {
		_ = db.Where("session_hash = ?", crypto.HashTokenSHA256(rawID)).Delete(&model.UserSession{}).Error
	}
}

func trackedSessionValid(c *gin.Context, rawID string, user *model.User) bool {
	db := database.GetDB()
	if db == nil {
		return false
	}
	var row model.UserSession
	if err := db.Where("session_hash = ? AND user_id = ?", crypto.HashTokenSHA256(rawID), user.Id).First(&row).Error; err != nil {
		return false
	}
	now := time.Now().UnixMilli()
	if row.LoginEpoch != user.LoginEpoch || row.ExpiresAt <= now {
		_ = db.Where("id = ?", row.ID).Delete(&model.UserSession{}).Error
		return false
	}
	if now-row.LastSeenAt >= trackedTouchEvery.Milliseconds() {
		_ = db.Model(&model.UserSession{}).Where("id = ?", row.ID).Update("last_seen_at", now).Error
	}
	return true
}

// ListTrackedSessions returns only session metadata; the bearer value is never
// persisted or returned. The caller must scope this to the authenticated user.
func ListTrackedSessions(userID int) ([]model.UserSession, error) {
	if userID <= 0 {
		return nil, nil
	}
	var rows []model.UserSession
	err := database.GetDB().Where("user_id = ?", userID).Order("last_seen_at DESC").Find(&rows).Error
	return rows, err
}

func RevokeTrackedSession(userID, sessionID int) error {
	if userID <= 0 || sessionID <= 0 {
		return nil
	}
	return database.GetDB().Where("id = ? AND user_id = ?", sessionID, userID).Delete(&model.UserSession{}).Error
}

// SetRecentAuthentication creates the short reauthentication proof after a
// successful password/TOTP login or explicit /reauth request. It stays inside
// the signed session cookie and is not an API-token capability.
func SetRecentAuthentication(c *gin.Context) {
	sessions.Default(c).Set(recentAuthKey, time.Now().UnixMilli())
}

func HasRecentAuthentication(c *gin.Context) bool {
	at := int64Value(sessions.Default(c).Get(recentAuthKey))
	return at > 0 && time.Now().UnixMilli()-at >= 0 && time.Now().UnixMilli()-at <= reauthTTL.Milliseconds()
}
