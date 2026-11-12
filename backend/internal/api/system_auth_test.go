package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"pennyharvester/backend/internal/api"
	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/store"
)

func newSystemRouter(t *testing.T) (http.Handler, *store.SQLite) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repository, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	registry := apiTestRegistry(t, testProvider(), nil)
	instance, err := engine.New(context.Background(), repository, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewRouter(instance, "", api.NewAuthenticator(repository)), repository
}

func requestWithCookie(t *testing.T, handler http.Handler, method, path string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func sessionCookies(t *testing.T, recorder *httptest.ResponseRecorder) []*http.Cookie {
	t.Helper()
	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected a session cookie, got none (status %d)", recorder.Code)
	}
	return cookies
}

func TestAccessPasswordLifecycle(t *testing.T) {
	handler, _ := newSystemRouter(t)

	status := request(t, handler, http.MethodGet, "/api/v1/auth/status", nil)
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"required":false`)) {
		t.Fatalf("unexpected initial status: %d %s", status.Code, status.Body.String())
	}

	// First-time setup needs no current password because protection is off.
	set := request(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "", "new_password": "{{PASSWORD_wy5qfw7m}}",
	})
	if set.Code != http.StatusNoContent {
		t.Fatalf("setting the password failed: %d %s", set.Code, set.Body.String())
	}

	if got := request(t, handler, http.MethodGet, "/api/v1/accounts", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("protected endpoint returned %d without a session", got)
	}

	wrong := request(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{"password": "nope"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password returned %d", wrong.Code)
	}

	login := request(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{"password": "{{PASSWORD_wy5qfw7m}}"})
	if login.Code != http.StatusNoContent {
		t.Fatalf("login failed: %d %s", login.Code, login.Body.String())
	}
	firstSession := sessionCookies(t, login)
	if got := requestWithCookie(t, handler, http.MethodGet, "/api/v1/accounts", nil, firstSession).Code; got != http.StatusOK {
		t.Fatalf("authenticated request returned %d", got)
	}

	// A password change revokes every session and issues a fresh one.
	change := requestWithCookie(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "{{PASSWORD_wy5qfw7m}}", "new_password": "{{PASSWORD_ckf7wnhj}}",
	}, firstSession)
	if change.Code != http.StatusNoContent {
		t.Fatalf("changing the password failed: %d %s", change.Code, change.Body.String())
	}
	if got := requestWithCookie(t, handler, http.MethodGet, "/api/v1/accounts", nil, firstSession).Code; got != http.StatusUnauthorized {
		t.Fatalf("the old session survived a password change: %d", got)
	}
	secondSession := sessionCookies(t, change)
	if got := requestWithCookie(t, handler, http.MethodGet, "/api/v1/accounts", nil, secondSession).Code; got != http.StatusOK {
		t.Fatalf("the reissued session was rejected: %d", got)
	}

	badCurrent := requestWithCookie(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "wrong", "new_password": "{{PASSWORD_2dbsyuyy}}",
	}, secondSession)
	if badCurrent.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password returned %d", badCurrent.Code)
	}

	disable := requestWithCookie(t, handler, http.MethodPost, "/api/v1/auth/password/disable", map[string]string{
		"current_password": "{{PASSWORD_ckf7wnhj}}",
	}, secondSession)
	if disable.Code != http.StatusNoContent {
		t.Fatalf("disabling the password failed: %d %s", disable.Code, disable.Body.String())
	}
	if got := request(t, handler, http.MethodGet, "/api/v1/accounts", nil).Code; got != http.StatusOK {
		t.Fatalf("protection was still enforced after disabling: %d", got)
	}
	status = request(t, handler, http.MethodGet, "/api/v1/auth/status", nil)
	if !bytes.Contains(status.Body.Bytes(), []byte(`"required":false`)) {
		t.Fatalf("status after disabling: %s", status.Body.String())
	}
}

func TestAccessPasswordRejectsShortSecret(t *testing.T) {
	handler, _ := newSystemRouter(t)
	short := request(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "", "new_password": "short",
	})
	if short.Code != http.StatusBadRequest {
		t.Fatalf("short password returned %d: %s", short.Code, short.Body.String())
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	handler, _ := newSystemRouter(t)
	set := request(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "", "new_password": "{{PASSWORD_wy5qfw7m}}",
	})
	if set.Code != http.StatusNoContent {
		t.Fatalf("setup failed: %d", set.Code)
	}
	for attempt := 0; attempt < 5; attempt++ {
		if code := request(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{"password": "wrong"}).Code; code != http.StatusUnauthorized {
			t.Fatalf("attempt %d returned %d", attempt, code)
		}
	}
	if code := request(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{"password": "{{PASSWORD_wy5qfw7m}}"}).Code; code != http.StatusTooManyRequests {
		t.Fatalf("expected rate limiting, got %d", code)
	}
}

type systemResponse struct {
	Proxy         netproxy.Config           `json:"proxy"`
	DataSource    engine.DataSourceSettings `json:"data_source"`
	Live          engine.LiveSettings       `json:"live"`
	Runtime       engine.SystemRuntime      `json:"runtime"`
	PasswordSet   bool                      `json:"password_set"`
	ProxyProbeURL string                    `json:"proxy_probe_url"`
	Environment   struct {
		HTTPProxySet  bool   `json:"http_proxy_set"`
		HTTPSProxySet bool   `json:"https_proxy_set"`
		NoProxy       string `json:"no_proxy"`
	} `json:"environment"`
}

func TestSystemSettingsProxyPersistence(t *testing.T) {
	handler, repository := newSystemRouter(t)
	ctx := context.Background()

	initial := request(t, handler, http.MethodGet, "/api/v1/system", nil)
	if initial.Code != http.StatusOK {
		t.Fatalf("reading system settings failed: %d %s", initial.Code, initial.Body.String())
	}
	var current systemResponse
	if err := json.Unmarshal(initial.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Proxy.Mode != netproxy.ModeEnvironment {
		t.Fatalf("default proxy mode = %q", current.Proxy.Mode)
	}
	if current.PasswordSet {
		t.Fatalf("no password should be set on a fresh database")
	}

	const secretURL = "http://user:pass@127.0.0.1:7890"
	update := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"proxy": map[string]any{"mode": "custom", "url": secretURL, "no_proxy": "127.0.0.1,localhost"},
	})
	if update.Code != http.StatusOK {
		t.Fatalf("saving proxy settings failed: %d %s", update.Code, update.Body.String())
	}
	var saved systemResponse
	if err := json.Unmarshal(update.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Proxy.Mode != netproxy.ModeCustom || saved.Proxy.NoProxy != "127.0.0.1,localhost" {
		t.Fatalf("unexpected saved proxy: %+v", saved.Proxy)
	}
	if strings.Contains(saved.Proxy.URL, "pass@") || !strings.Contains(saved.Proxy.URL, netproxy.RedactedSecret) {
		t.Fatalf("proxy password was not redacted in the response: %q", saved.Proxy.URL)
	}

	stored, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Proxy.URL != secretURL {
		t.Fatalf("stored proxy URL = %q, want the full URL with secret", stored.Proxy.URL)
	}

	// Echoing the redacted URL back must keep the stored secret.
	reupdate := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"proxy": map[string]any{"mode": "custom", "url": saved.Proxy.URL, "no_proxy": "127.0.0.1"},
	})
	if reupdate.Code != http.StatusOK {
		t.Fatalf("updating with the redacted URL failed: %d %s", reupdate.Code, reupdate.Body.String())
	}
	stored, err = repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Proxy.URL != secretURL {
		t.Fatalf("redacted update wiped the secret: %q", stored.Proxy.URL)
	}

	invalid := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"proxy": map[string]any{"mode": "custom", "url": "ftp://127.0.0.1:21"},
	})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid proxy was accepted: %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestSystemSettingsLiveCredentialsAreRedactedAndPreserved(t *testing.T) {
	handler, repository := newSystemRouter(t)
	ctx := context.Background()
	key := "0x" + strings.Repeat("1", 64)
	funder := "0x0000000000000000000000000000000000000002"

	update := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"data_source": map[string]any{
			"gamma_base_url": "https://gamma.example.com",
			"clob_base_url":  "https://clob.example.com",
		},
		"live": map[string]any{
			"private_key":    key,
			"funder":         funder,
			"signature_type": 1,
			"api_key":        "api-key",
			"api_secret":     "api-secret",
			"api_passphrase": "api-passphrase",
		},
	})
	if update.Code != http.StatusOK {
		t.Fatalf("saving live settings failed: %d %s", update.Code, update.Body.String())
	}
	var saved systemResponse
	if err := json.Unmarshal(update.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.DataSource.GammaBaseURL != "https://gamma.example.com" || saved.DataSource.CLOBBaseURL != "https://clob.example.com" {
		t.Fatalf("data source response = %+v", saved.DataSource)
	}
	if saved.Live.PrivateKey != netproxy.RedactedSecret || saved.Live.APISecret != netproxy.RedactedSecret || saved.Live.APIPassphrase != netproxy.RedactedSecret {
		t.Fatalf("secrets were not redacted: %+v", saved.Live)
	}
	if saved.Live.APIKey != netproxy.RedactedSecret {
		t.Fatalf("API key should be masked, got %q", saved.Live.APIKey)
	}

	stored, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Live.PrivateKey != key || stored.Live.APISecret != "api-secret" || stored.Live.APIPassphrase != "api-passphrase" {
		t.Fatalf("stored live settings = %+v", stored.Live)
	}
	if stored.DataSource.CLOBBaseURL != "https://clob.example.com" {
		t.Fatalf("stored data source = %+v", stored.DataSource)
	}

	// Echoing masked secrets back must keep the stored values while allowing
	// the non-secret fields to change.
	reupdate := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"live": map[string]any{
			"private_key":    saved.Live.PrivateKey,
			"funder":         funder,
			"signature_type": 1,
			"api_key":        saved.Live.APIKey,
			"api_secret":     saved.Live.APISecret,
			"api_passphrase": saved.Live.APIPassphrase,
		},
	})
	if reupdate.Code != http.StatusOK {
		t.Fatalf("updating with masked secrets failed: %d %s", reupdate.Code, reupdate.Body.String())
	}
	stored, err = repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Live.PrivateKey != key || stored.Live.APISecret != "api-secret" {
		t.Fatalf("masked update wiped secrets: %+v", stored.Live)
	}

	// Saving another section must not wipe the live credentials.
	proxyUpdate := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"proxy": map[string]any{"mode": "off"},
	})
	if proxyUpdate.Code != http.StatusOK {
		t.Fatalf("proxy-only update failed: %d %s", proxyUpdate.Code, proxyUpdate.Body.String())
	}
	stored, err = repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Live.PrivateKey != key || stored.DataSource.GammaBaseURL != "https://gamma.example.com" {
		t.Fatalf("partial update wiped other sections: %+v", stored)
	}
}

func TestProxyTestReportsFailure(t *testing.T) {
	handler, _ := newSystemRouter(t)
	result := request(t, handler, http.MethodPost, "/api/v1/system/proxy/test", map[string]any{
		"proxy": map[string]any{"mode": "custom", "url": "http://127.0.0.1:1"},
	})
	if result.Code != http.StatusOK {
		t.Fatalf("proxy test endpoint returned %d: %s", result.Code, result.Body.String())
	}
	var payload struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK || payload.Error == "" {
		t.Fatalf("expected a failed probe, got %s", result.Body.String())
	}
}

func TestLiveProbeRejectsPartialCredentials(t *testing.T) {
	handler, _ := newSystemRouter(t)
	response := request(t, handler, http.MethodPost, "/api/v1/system/live/test", map[string]any{
		"live": map[string]any{"api_key": "only-key"},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("partial credentials must be rejected: %d %s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("必须同时填写")) {
		t.Fatalf("unexpected error body: %s", response.Body.String())
	}
}

func TestRelayerProbeRejectsInvalidLiveSettings(t *testing.T) {
	handler, _ := newSystemRouter(t)
	response := request(t, handler, http.MethodPost, "/api/v1/system/relayer/test", map[string]any{
		"live":            map[string]any{"api_key": "only-key"},
		"relayer_api_key": "01967c03-b8c8-7000-8f68-8b8eaec6fd3d",
		"relayer_address": "0x0000000000000000000000000000000000000001",
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid live settings must be rejected: %d %s", response.Code, response.Body.String())
	}
}

func TestSystemSettingsVenueBlocksRoundTrip(t *testing.T) {
	handler, repository := newSystemRouter(t)
	ctx := context.Background()
	key := "0x" + strings.Repeat("2", 64)
	wallet := "0x" + strings.Repeat("3", 40)

	update := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"venues": map[string]any{
			"predictfun": map[string]any{
				"data_source": map[string]any{"base_url": "https://api.predict.fun"},
				"live": map[string]any{
					"api_key": "pf-key", "private_key": key, "smart_wallet": wallet, "testnet": true,
				},
			},
		},
	})
	if update.Code != http.StatusOK {
		t.Fatalf("saving predictfun settings failed: %d %s", update.Code, update.Body.String())
	}
	var saved struct {
		Venues map[market.Venue]struct {
			DataSource engine.VenueDataSourceSettings `json:"data_source"`
			Live       engine.VenueLiveSettings       `json:"live"`
			LiveOK     bool                           `json:"live_configured"`
			KeyOK      bool                           `json:"private_key_configured"`
			APIKeyOK   bool                           `json:"api_key_configured"`
		} `json:"venues"`
		Runtime engine.SystemRuntime `json:"runtime"`
	}
	if err := json.Unmarshal(update.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	pf := saved.Venues[market.VenuePredictFun]
	if pf.DataSource.BaseURL != "https://api.predict.fun" || pf.Live.APIKey != netproxy.RedactedSecret || !testnetOn(pf.Live.Testnet) {
		t.Fatalf("predictfun venue block was not returned: %+v", pf)
	}
	if pf.Live.PrivateKey != netproxy.RedactedSecret || !pf.LiveOK || !pf.KeyOK || !pf.APIKeyOK {
		t.Fatalf("predictfun secrets must be redacted with configured flags: %+v", pf)
	}
	if len(saved.Runtime.Venues) != 1 || saved.Runtime.Venues[0].Venue != market.VenuePolymarket {
		t.Fatalf("runtime must report one row per registered venue: %+v", saved.Runtime.Venues)
	}

	stored, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pfStored := stored.Venues[market.VenuePredictFun]
	if pfStored.Live.PrivateKey != key || pfStored.Live.SmartWallet != wallet || !testnetOn(pfStored.Live.Testnet) {
		t.Fatalf("stored predictfun credentials = %+v", pfStored.Live)
	}
	if pfStored.DataSource.BaseURL != "https://api.predict.fun" {
		t.Fatalf("stored predictfun data source = %+v", pfStored.DataSource)
	}

	// A flat-only update (old frontend) must keep working and be mirrored into
	// the venues.polymarket view. The key must differ from Predict.fun's.
	pmKey := "0x" + strings.Repeat("4", 64)
	update = request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
		"live": map[string]any{
			"private_key": pmKey, "api_key": "pm-key", "api_secret": "pm-secret",
			"api_passphrase": "pm-passphrase", "signature_type": 0,
		},
	})
	if update.Code != http.StatusOK {
		t.Fatalf("flat live update failed: %d %s", update.Code, update.Body.String())
	}
	stored, err = repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pmStored := stored.Venues[market.VenuePolymarket]
	if pmStored.Live.PrivateKey != pmKey || pmStored.Live.APIKey != "pm-key" || pmStored.Live.APISecret != "pm-secret" {
		t.Fatalf("venues.polymarket must mirror the flat fields: %+v", pmStored.Live)
	}
	var response struct {
		Venues map[market.Venue]struct {
			Live engine.VenueLiveSettings `json:"live"`
		} `json:"venues"`
	}
	if err := json.Unmarshal(update.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Venues[market.VenuePolymarket].Live.APISecret != netproxy.RedactedSecret {
		t.Fatalf("venues.polymarket must not echo secrets: %+v", response.Venues)
	}
}

// testnetOn reads the tri-state network flag; nil means "inherit the process
// environment", which in these tests is mainnet.
func testnetOn(v *bool) bool { return v != nil && *v }
