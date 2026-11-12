package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type memoryPasswordStore struct {
	mu   sync.Mutex
	hash string
}

func (s *memoryPasswordStore) LoadAccessHash(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hash, nil
}

func (s *memoryPasswordStore) SaveAccessHash(_ context.Context, hash string) error {
	s.mu.Lock()
	s.hash = hash
	s.mu.Unlock()
	return nil
}

func TestSameOriginGuardRejectsCrossSiteWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name        string
		method      string
		host        string
		origin      string
		fetchSite   string
		contentType string
		body        string
		want        int
	}{
		{name: "foreign origin", method: http.MethodPost, host: "127.0.0.1:8080", origin: "https://evil.example", want: http.StatusForbidden},
		{name: "origin host mismatch", method: http.MethodPost, host: "127.0.0.1:8080", origin: "http://127.0.0.1:9090", want: http.StatusForbidden},
		{name: "fetch metadata", method: http.MethodPost, host: "127.0.0.1:8080", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "same origin", method: http.MethodPost, host: "127.0.0.1:8080", origin: "http://127.0.0.1:8080", want: http.StatusNoContent},
		{name: "non-browser client", method: http.MethodPost, host: "127.0.0.1:8080", want: http.StatusNoContent},
		{name: "form body", method: http.MethodPut, host: "127.0.0.1:8080", contentType: "application/x-www-form-urlencoded", body: "a=b", want: http.StatusUnsupportedMediaType},
		{name: "text body", method: http.MethodPut, host: "127.0.0.1:8080", contentType: "text/plain", body: "{}", want: http.StatusUnsupportedMediaType},
		{name: "json body", method: http.MethodPut, host: "127.0.0.1:8080", contentType: "application/json", body: "{}", want: http.StatusNoContent},
		{name: "get is safe", method: http.MethodGet, host: "127.0.0.1:8080", origin: "https://evil.example", want: http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(sameOriginGuard())
			called := false
			router.Any("/api/v1/test", func(c *gin.Context) {
				called = true
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/v1/test", nil)
			request.Host = tc.host
			if tc.body != "" {
				request = httptest.NewRequest(tc.method, "http://"+tc.host+"/api/v1/test", strings.NewReader(tc.body))
				request.Host = tc.host
			}
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			if tc.contentType != "" {
				request.Header.Set("Content-Type", tc.contentType)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.want, recorder.Body.String())
			}
			if tc.want != http.StatusNoContent && called {
				t.Fatal("handler ran for a rejected request")
			}
		})
	}
}

func TestAllowedOriginHost(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", "app.internal, 10.0.0.5:8080")
	cases := map[string]bool{
		"localhost": true, "127.0.0.1": true, "::1": true,
		"app.internal": true, "10.0.0.5": true, "evil.example": false,
	}
	for host, want := range cases {
		if got := allowedOriginHost(host); got != want {
			t.Fatalf("allowedOriginHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestTrustedProxiesFromEnv(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	if got := trustedProxiesFromEnv(); got != nil {
		t.Fatalf("empty TRUSTED_PROXIES = %v, want nil", got)
	}
	t.Setenv("TRUSTED_PROXIES", "127.0.0.1, 10.0.0.0/8")
	got := trustedProxiesFromEnv()
	if len(got) != 2 || got[0] != "127.0.0.1" || got[1] != "10.0.0.0/8" {
		t.Fatalf("trustedProxiesFromEnv() = %v", got)
	}
}

func TestLoginLimitCountsBeforeVerification(t *testing.T) {
	auth := NewAuthenticator(&memoryPasswordStore{})
	auth.now = func() time.Time { return time.Unix(1000, 0) }
	for i := 0; i < maxLoginFailures; i++ {
		if !auth.reserveAttempt("1.2.3.4") {
			t.Fatalf("attempt %d rejected too early", i+1)
		}
	}
	if auth.reserveAttempt("1.2.3.4") {
		t.Fatal("attempt beyond the limit was admitted")
	}
	auth.clearFailures("1.2.3.4")
	if !auth.reserveAttempt("1.2.3.4") {
		t.Fatal("successful login did not clear the window")
	}
}

func TestSessionIssuedAtStaleRevisionIsRejected(t *testing.T) {
	store := &memoryPasswordStore{}
	auth := NewAuthenticator(store)
	if _, err := auth.SetPasswordRevision(context.Background(), "", "initial-password"); err != nil {
		t.Fatal(err)
	}
	// Simulate a verification that started before a concurrent password change.
	_, staleRevision, err := auth.verifyRevision(context.Background(), "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetPasswordRevision(context.Background(), "initial-password", "changed-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.newSessionAt(staleRevision); err == nil {
		t.Fatal("stale verification issued a session after the password change")
	}
	current, err := auth.SetPasswordRevision(context.Background(), "changed-password", "third-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.newSessionAt(current); err != nil {
		t.Fatalf("current revision rejected: %v", err)
	}
}
