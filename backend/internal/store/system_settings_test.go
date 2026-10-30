package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/netproxy"
)

func intPointer(value int) *int { return &value }

func TestSystemSettingsSecretsAreEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.db")
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	key := "0x" + strings.Repeat("1", 64)
	settings := engine.SystemSettings{
		Proxy: netproxy.Config{Mode: netproxy.ModeCustom, URL: "http://user:proxy-secret@127.0.0.1:7890"},
		DataSource: engine.DataSourceSettings{
			GammaBaseURL: "https://gamma.example.com",
			CLOBBaseURL:  "https://clob.example.com",
		},
		Live: engine.LiveSettings{
			PrivateKey:    key,
			Funder:        "0x0000000000000000000000000000000000000002",
			SignatureType: intPointer(1),
			APIKey:        "api-key",
			APISecret:     "api-secret",
			APIPassphrase: "api-passphrase",
		},
	}
	if err := repository.SaveSystemSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}

	var payload string
	if err := repository.db.QueryRowContext(ctx, "SELECT payload FROM system_settings WHERE id = 1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key, "api-secret", "api-passphrase", settings.Proxy.URL} {
		if strings.Contains(payload, secret) {
			t.Fatalf("stored payload leaks secret %q: %s", secret, payload)
		}
	}
	if !strings.Contains(payload, "enc:v1:") {
		t.Fatalf("stored payload is not encrypted: %s", payload)
	}

	loaded, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Live.PrivateKey != key || loaded.Live.APISecret != "api-secret" || loaded.Live.APIPassphrase != "api-passphrase" {
		t.Fatalf("decrypted live settings = %+v", loaded.Live)
	}
	if loaded.Proxy.URL != settings.Proxy.URL {
		t.Fatalf("decrypted proxy URL = %q", loaded.Proxy.URL)
	}

	// The key file must survive a restart so the same database stays readable.
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err = reopened.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Live.PrivateKey != key {
		t.Fatalf("private key after reopen = %q", loaded.Live.PrivateKey)
	}
}

func TestLegacyPlaintextSystemSettingsAreReencrypted(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	key := "0x" + strings.Repeat("2", 64)
	legacy := `{"proxy":{"mode":"custom","url":"http://user:legacy-secret@127.0.0.1:7890","no_proxy":""},"live":{"private_key":"` + key + `","funder":"","signature_type":null,"api_key":"","api_secret":"","api_passphrase":""}}`
	if _, err := repository.db.ExecContext(ctx, `INSERT INTO system_settings(id, payload, updated_at) VALUES(1, ?, ?)`, legacy, formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Live.PrivateKey != key {
		t.Fatalf("legacy private key = %q", loaded.Live.PrivateKey)
	}
	var payload string
	if err := repository.db.QueryRowContext(ctx, "SELECT payload FROM system_settings WHERE id = 1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, key) || strings.Contains(payload, "http://user:") {
		t.Fatalf("legacy secrets were not re-encrypted: %s", payload)
	}
}

func TestSystemSettingsAndAccessHash(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	defaults, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Proxy.Mode != netproxy.ModeEnvironment {
		t.Fatalf("default proxy mode = %q", defaults.Proxy.Mode)
	}

	settings := engine.SystemSettings{Proxy: netproxy.Config{
		Mode: netproxy.ModeCustom, URL: "socks5://user:pass@127.0.0.1:1080", NoProxy: ".local",
	}}
	if err := repository.SaveSystemSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proxy != settings.Proxy {
		t.Fatalf("loaded proxy = %+v, want %+v", loaded.Proxy, settings.Proxy)
	}

	hash, err := repository.LoadAccessHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		t.Fatalf("fresh database returned hash %q", hash)
	}
	if err := repository.SaveAccessHash(ctx, "$2a$10$examplehash"); err != nil {
		t.Fatal(err)
	}
	if hash, err = repository.LoadAccessHash(ctx); err != nil || hash != "$2a$10$examplehash" {
		t.Fatalf("hash = %q, err = %v", hash, err)
	}
	if err := repository.SaveAccessHash(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if hash, err = repository.LoadAccessHash(ctx); err != nil || hash != "" {
		t.Fatalf("hash after clearing = %q, err = %v", hash, err)
	}
}
