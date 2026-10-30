package netproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateModes(t *testing.T) {
	cases := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{"default", DefaultConfig(), false},
		{"off", Config{Mode: ModeOff}, false},
		{"custom http", Config{Mode: ModeCustom, URL: "http://127.0.0.1:7890"}, false},
		{"custom socks", Config{Mode: ModeCustom, URL: "socks5://user:pass@host:1080"}, false},
		{"custom without url", Config{Mode: ModeCustom}, true},
		{"custom bad scheme", Config{Mode: ModeCustom, URL: "ftp://host:21"}, true},
		{"custom bad url", Config{Mode: ModeCustom, URL: "://host"}, true},
		{"custom no_proxy with scheme", Config{Mode: ModeCustom, URL: "http://host:1", NoProxy: "http://x"}, true},
		{"unknown mode", Config{Mode: "sometimes"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for %+v", tc.config)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRedactAndMergeSecret(t *testing.T) {
	stored := Config{Mode: ModeCustom, URL: "http://user:secret@127.0.0.1:7890"}
	redacted := stored.Redacted()
	want := "http://user:" + RedactedSecret + "@127.0.0.1:7890"
	if redacted.URL != want {
		t.Fatalf("redacted URL = %q, want %q", redacted.URL, want)
	}
	if merged := redacted.MergeSecret(stored); merged.URL != stored.URL {
		t.Fatalf("merged URL = %q, want the stored secret", merged.URL)
	}
	changed := Config{Mode: ModeCustom, URL: "http://user:new-secret@127.0.0.1:7891"}
	if merged := changed.MergeSecret(stored); merged.URL != changed.URL {
		t.Fatalf("a freshly typed URL must not be replaced: %q", merged.URL)
	}
}

func TestCustomProxyRoutesRequests(t *testing.T) {
	var seen string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxy.Close()

	manager, err := New(Config{Mode: ModeCustom, URL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.HTTPClient(5 * time.Second).Get("http://polymarket.invalid/ping")
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if seen != "http://polymarket.invalid/ping" {
		t.Fatalf("proxy saw %q", seen)
	}
	if manager.ProxyURL() == nil || manager.ProxyURL().Host != proxy.Listener.Addr().String() {
		t.Fatalf("ProxyURL = %v", manager.ProxyURL())
	}
}

func TestNoProxyBypassesCustomProxy(t *testing.T) {
	hits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	manager, err := New(Config{Mode: ModeCustom, URL: proxy.URL, NoProxy: "127.0.0.1,localhost"})
	if err != nil {
		t.Fatal(err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	response, err := manager.HTTPClient(5 * time.Second).Get(target.URL)
	if err != nil {
		t.Fatalf("direct request: %v", err)
	}
	response.Body.Close()
	if hits != 0 {
		t.Fatalf("NO_PROXY host was sent to the proxy %d times", hits)
	}
}

func TestApplyRebuildsClients(t *testing.T) {
	manager, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if manager.ProxyURL() != nil {
		t.Fatalf("environment mode must not report an explicit proxy")
	}
	first := manager.HTTPClient(5 * time.Second)
	if err := manager.Apply(Config{Mode: ModeCustom, URL: "http://127.0.0.1:7890"}); err != nil {
		t.Fatal(err)
	}
	second := manager.HTTPClient(5 * time.Second)
	if first == second {
		t.Fatalf("client must be rebuilt after the configuration changes")
	}
	if manager.Config().Mode != ModeCustom {
		t.Fatalf("mode = %s", manager.Config().Mode)
	}
	if err := manager.Apply(Config{Mode: ModeOff}); err != nil {
		t.Fatal(err)
	}
	transport := manager.HTTPClient(5 * time.Second).Transport.(*http.Transport)
	request, _ := http.NewRequest(http.MethodGet, "http://polymarket.invalid/ping", nil)
	proxyForRequest, err := transport.Proxy(request)
	if err != nil {
		t.Fatal(err)
	}
	if proxyForRequest != nil {
		t.Fatalf("off mode must connect directly, got %v", proxyForRequest)
	}
}
