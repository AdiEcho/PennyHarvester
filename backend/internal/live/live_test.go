package live

import (
	"strings"
	"testing"
)

func TestExecutorConfigurationValidation(t *testing.T) {
	if New(Config{}).Configured() {
		t.Fatal("an executor without a private key must not be configured")
	}
	if !strings.Contains(New(Config{}).ReadyError(), "POLYMARKET_PRIVATE_KEY") {
		t.Fatal("missing private key should be reported")
	}

	key := "0x" + strings.Repeat("1", 64)
	proxy := New(Config{PrivateKey: key, SignatureType: 1})
	if !proxy.Configured() {
		t.Fatal("private key should mark the executor configured")
	}
	if !strings.Contains(proxy.ReadyError(), "POLYMARKET_FUNDER") {
		t.Fatalf("proxy signature type without funder should fail: %s", proxy.ReadyError())
	}

	invalidType := New(Config{PrivateKey: key, SignatureType: 4})
	if !strings.Contains(invalidType.ReadyError(), "SIGNATURE_TYPE") {
		t.Fatalf("invalid signature type should fail: %s", invalidType.ReadyError())
	}

	valid := New(Config{PrivateKey: key, SignatureType: 0})
	if err := valid.ReadyError(); err != "" {
		t.Fatalf("valid EOA config should be ready: %s", err)
	}
	if valid.Signer() == "" {
		t.Fatal("signer address should be derived locally")
	}
}
