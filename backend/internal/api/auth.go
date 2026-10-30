package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "ph_session"
	sessionTTL        = 7 * 24 * time.Hour
	minPasswordBytes  = 8
	maxPasswordBytes  = 72 // bcrypt truncates beyond 72 bytes
	loginWindow       = time.Minute
	maxLoginFailures  = 5
)

var (
	// ErrPasswordNotSet is returned when a password operation needs an
	// existing access password but password protection is off.
	ErrPasswordNotSet = errors.New("当前未启用访问密码")
	// ErrAccessDenied is returned when the supplied password does not match.
	ErrAccessDenied = errors.New("访问密码错误")
	// ErrPasswordPolicy is returned when the new password is unusable.
	ErrPasswordPolicy = errors.New("新密码长度需为 8–72 个字符，且不能全为空白")
)

// PasswordStore persists the bcrypt hash of the web access password. An empty
// hash disables password protection, which keeps the historical
// "localhost-only, no login" default for existing installs.
type PasswordStore interface {
	LoadAccessHash(context.Context) (string, error)
	SaveAccessHash(context.Context, string) error
}

type Authenticator struct {
	store PasswordStore

	mu       sync.Mutex
	hash     string
	loaded   bool
	sessions map[string]time.Time
	failures map[string][]time.Time
	now      func() time.Time
}

func NewAuthenticator(store PasswordStore) *Authenticator {
	return &Authenticator{
		store:    store,
		sessions: map[string]time.Time{},
		failures: map[string][]time.Time{},
		now:      time.Now,
	}
}

func (a *Authenticator) ensureLoaded(ctx context.Context) error {
	a.mu.Lock()
	loaded := a.loaded
	a.mu.Unlock()
	if loaded {
		return nil
	}
	hash, err := a.store.LoadAccessHash(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.hash = strings.TrimSpace(hash)
	a.loaded = true
	a.mu.Unlock()
	return nil
}

// Enabled reports whether a login is required.
func (a *Authenticator) Enabled(ctx context.Context) (bool, error) {
	if err := a.ensureLoaded(ctx); err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hash != "", nil
}

// Verify checks a candidate password. When no password is configured it
// succeeds, so callers can use it for the first-time setup flow.
func (a *Authenticator) Verify(ctx context.Context, password string) (bool, error) {
	if err := a.ensureLoaded(ctx); err != nil {
		return false, err
	}
	a.mu.Lock()
	hash := a.hash
	a.mu.Unlock()
	if hash == "" {
		return true, nil
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return false, nil
	}
	return true, nil
}

// SetPassword verifies the current password (if any) and stores a new hash.
// All sessions are dropped afterwards; the caller issues a fresh one.
func (a *Authenticator) SetPassword(ctx context.Context, current, next string) error {
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	if len(next) < minPasswordBytes || len(next) > maxPasswordBytes || strings.TrimSpace(next) == "" {
		return ErrPasswordPolicy
	}
	ok, err := a.Verify(ctx, current)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAccessDenied
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := a.store.SaveAccessHash(ctx, string(hash)); err != nil {
		return err
	}
	a.mu.Lock()
	a.hash = string(hash)
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	return nil
}

// Disable turns password protection off after verifying the current password.
func (a *Authenticator) Disable(ctx context.Context, current string) error {
	enabled, err := a.Enabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrPasswordNotSet
	}
	ok, err := a.Verify(ctx, current)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAccessDenied
	}
	if err := a.store.SaveAccessHash(ctx, ""); err != nil {
		return err
	}
	a.mu.Lock()
	a.hash = ""
	a.sessions = map[string]time.Time{}
	a.mu.Unlock()
	return nil
}

func (a *Authenticator) newSession() (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := a.now().Add(sessionTTL)
	a.mu.Lock()
	a.sessions[token] = expires
	a.mu.Unlock()
	return token, expires, nil
}

func (a *Authenticator) Valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[token]
	if !ok {
		return false
	}
	if !a.now().Before(expires) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func (a *Authenticator) Revoke(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

func (a *Authenticator) allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := a.now().Add(-loginWindow)
	recent := a.failures[ip][:0]
	for _, at := range a.failures[ip] {
		if at.After(cutoff) {
			recent = append(recent, at)
		}
	}
	if len(recent) == 0 {
		delete(a.failures, ip)
	} else {
		a.failures[ip] = recent
	}
	return len(recent) < maxLoginFailures
}

func (a *Authenticator) recordFailure(ip string) {
	a.mu.Lock()
	a.failures[ip] = append(a.failures[ip], a.now())
	a.mu.Unlock()
}

func (a *Authenticator) clearFailures(ip string) {
	a.mu.Lock()
	delete(a.failures, ip)
	a.mu.Unlock()
}

// Middleware rejects unauthenticated API calls when password protection is on.
func (a *Authenticator) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		enabled, err := a.Enabled(c.Request.Context())
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "读取访问密码状态失败: " + err.Error()})
			return
		}
		if !enabled {
			c.Next()
			return
		}
		token, _ := c.Cookie(sessionCookieName)
		if !a.Valid(token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "需要登录后才能访问"})
			return
		}
		c.Next()
	}
}

func (h *Handler) authStatus(c *gin.Context) {
	enabled, err := h.auth.Enabled(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	token, _ := c.Cookie(sessionCookieName)
	c.JSON(http.StatusOK, gin.H{
		"required":      enabled,
		"authenticated": !enabled || h.auth.Valid(token),
	})
}

func (h *Handler) login(c *gin.Context) {
	enabled, err := h.auth.Enabled(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	if !enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前未启用访问密码，无需登录"})
		return
	}
	var request struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "登录请求无效: " + err.Error()})
		return
	}
	ip := c.ClientIP()
	if !h.auth.allow(ip) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录尝试过于频繁，请稍后再试"})
		return
	}
	ok, err := h.auth.Verify(c.Request.Context(), request.Password)
	if err != nil {
		respondError(c, err)
		return
	}
	if !ok {
		h.auth.recordFailure(ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "访问密码错误"})
		return
	}
	h.auth.clearFailures(ip)
	token, expires, err := h.auth.newSession()
	if err != nil {
		respondError(c, err)
		return
	}
	h.setSessionCookie(c, token, expires)
	c.Status(http.StatusNoContent)
}

func (h *Handler) logout(c *gin.Context) {
	token, _ := c.Cookie(sessionCookieName)
	h.auth.Revoke(token)
	h.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

func (h *Handler) updatePassword(c *gin.Context) {
	var request struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "密码请求无效: " + err.Error()})
		return
	}
	if err := h.auth.SetPassword(c.Request.Context(), request.CurrentPassword, request.NewPassword); err != nil {
		respondAuthError(c, err)
		return
	}
	// Changing the password drops every session, including the caller's, so
	// hand the caller a fresh one to avoid an immediate logout.
	token, expires, err := h.auth.newSession()
	if err != nil {
		respondError(c, err)
		return
	}
	h.setSessionCookie(c, token, expires)
	c.Status(http.StatusNoContent)
}

func (h *Handler) disablePassword(c *gin.Context) {
	var request struct {
		CurrentPassword string `json:"current_password"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "密码请求无效: " + err.Error()})
		return
	}
	if err := h.auth.Disable(c.Request.Context(), request.CurrentPassword); err != nil {
		respondAuthError(c, err)
		return
	}
	h.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

func respondAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrAccessDenied), errors.Is(err, ErrPasswordNotSet):
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
	case errors.Is(err, ErrPasswordPolicy):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		respondError(c, err)
	}
}

func (h *Handler) setSessionCookie(c *gin.Context, token string, expires time.Time) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(sessionCookieName, token, int(time.Until(expires).Seconds()), "/", "", c.Request.TLS != nil, true)
}

func (h *Handler) clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(sessionCookieName, "", -1, "/", "", c.Request.TLS != nil, true)
}
