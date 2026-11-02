package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/venue"
)

// credentialsApplied is one recorded ApplyLiveSettings call.
type credentialsApplied struct {
	apiKey      string
	privateKey  string
	smartWallet string
	testnet     string
	baseURL     string
}

// fakeCredentialLive is a venue executor that accepts credentials at runtime,
// like the Predict.fun one. The engine discovers the capability by interface
// assertion, so the fake has to spell the method exactly.
type fakeCredentialLive struct {
	*fakeLive
	applied []credentialsApplied
	// resets counts baseline restores. A probe must return the executor to its
	// startup state before re-applying saved credentials, otherwise an empty
	// stored wallet would keep the candidate's wallet.
	resets int
}

func (f *fakeCredentialLive) ApplyLiveSettings(apiKey, privateKey, smartWallet string, testnet *bool, baseURL string) error {
	// The real client rejects a key it cannot parse before swapping it, and the
	// engine's fail-closed behaviour depends on that. Mirror it so a test can
	// tell "the engine tried" from "the engine overwrote a working key".
	if privateKey != "" {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(privateKey, "0x"), "0X")
		if len(trimmed) != 64 || strings.Trim(trimmed, "0123456789abcdefABCDEF") != "" {
			return fmt.Errorf("实盘私钥必须是 0x 开头的 64 位十六进制字符串")
		}
	}
	f.applied = append(f.applied, credentialsApplied{apiKey: apiKey, privateKey: privateKey, smartWallet: smartWallet, testnet: testnetText(testnet), baseURL: baseURL})
	return nil
}

// ResetLiveSettings is the baseline restore the probe uses before re-applying
// saved credentials.
func (f *fakeCredentialLive) ResetLiveSettings() error {
	f.resets++
	return nil
}

// testnetText records the tri-state network flag in a comparable form.
func testnetText(v *bool) string {
	switch {
	case v == nil:
		return "inherit"
	case *v:
		return "testnet"
	default:
		return "mainnet"
	}
}

func (f *fakeCredentialLive) Signer() string { return "{{SECRET_iwvjkhwg}}" }
func (f *fakeCredentialLive) Maker() string  { return "{{SECRET_iwvjkhwg}}" }

const (
	storedKeyHex  = "0x" + "11" + "11111111111111111111111111111111111111111111111111111111111111"
	storedWallet  = "0x" + "2222222222222222222222222222222222222222"
	candidateKey  = "0x" + "33" + "33333333333333333333333333333333333333333333333333333333333333"
	otherWallet   = "0x" + "4444444444444444444444444444444444444444"
	predictFunURL = "https://predictfun.test"
)

// newCredentialEngine wires a registry whose Predict.fun executor accepts
// runtime credentials, plus one stored Predict.fun credential block.
func newCredentialEngine(t *testing.T) (*engine.Engine, *fakeCredentialLive) {
	t.Helper()
	live := &fakeCredentialLive{fakeLive: &fakeLive{balance: 50, allowance: 60}}
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, markets: map[string]market.Market{},
	}, live)
	instance, _ := newTestEngineWithRegistry(t, registry)
	ctx := context.Background()
	system := instance.SystemSettings()
	system.Venues[market.VenuePredictFun] = engine.VenueSystemSettings{
		DataSource: engine.VenueDataSourceSettings{BaseURL: predictFunURL},
		Live: engine.VenueLiveSettings{
			LiveSettings: engine.LiveSettings{APIKey: "stored-api-key", PrivateKey: storedKeyHex},
			SmartWallet:  storedWallet,
			Testnet:      boolPtr(true),
		},
	}
	if err := instance.UpdateSystemSettings(ctx, system); err != nil {
		t.Fatalf("save system settings: %v", err)
	}
	return instance, live
}

// TestSystemSettingsApplyPredictFunCredentials proves a saved Predict.fun block
// reaches the executor without a restart: the settings page is the only way an
// operator can enter the credentials, so a restart requirement would look like
// a broken save.
func TestSystemSettingsApplyPredictFunCredentials(t *testing.T) {
	_, live := newCredentialEngine(t)
	if len(live.applied) == 0 {
		t.Fatal("saving a Predict.fun block must reach the executor")
	}
	got := live.applied[len(live.applied)-1]
	want := credentialsApplied{apiKey: "stored-api-key", privateKey: storedKeyHex, smartWallet: storedWallet, testnet: "testnet", baseURL: predictFunURL}
	if got != want {
		t.Fatalf("applied %+v, want %+v", got, want)
	}
}

// TestAbsentPredictFunBlockKeepsProcessCredentials pins the inheritance rule:
// with no stored block the credentials come from the environment passed to the
// constructor, so the engine must not overwrite them with empty values.
func TestAbsentPredictFunBlockKeepsProcessCredentials(t *testing.T) {
	instance, live := newCredentialEngine(t)
	live.applied = nil
	ctx := context.Background()
	system := instance.SystemSettings()
	delete(system.Venues, market.VenuePredictFun)
	if err := instance.UpdateSystemSettings(ctx, system); err != nil {
		t.Fatalf("save system settings: %v", err)
	}
	if len(live.applied) != 0 {
		t.Fatalf("an absent venue block must not touch the executor, applied %+v", live.applied)
	}
}

// TestProbeLiveVenuePredictFunRestoresStoredCredentials proves the probe tries
// the candidate and then puts the stored credentials back, so testing a key
// never leaves the executor signing with something that is not saved.
func TestProbeLiveVenuePredictFunRestoresStoredCredentials(t *testing.T) {
	instance, live := newCredentialEngine(t)
	live.applied = nil
	candidate := engine.VenueLiveSettings{
		LiveSettings: engine.LiveSettings{APIKey: "candidate-key", PrivateKey: candidateKey},
		SmartWallet:  otherWallet,
	}
	probe, err := instance.ProbeLiveVenue(context.Background(), market.VenuePredictFun, candidate, "")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Cash != 50 || probe.Allowance != 60 {
		t.Fatalf("probe did not read the venue balance: %+v", probe)
	}
	if probe.Source != "chain" || probe.Signer == "" {
		t.Fatalf("probe must report the chain source and the signer: %+v", probe)
	}
	if len(live.applied) != 2 {
		t.Fatalf("expected a candidate apply and a restore, got %+v", live.applied)
	}
	first, last := live.applied[0], live.applied[1]
	if first.apiKey != "candidate-key" || first.privateKey != candidateKey || first.smartWallet != otherWallet {
		t.Fatalf("probe did not try the candidate: %+v", first)
	}
	if last.apiKey != "stored-api-key" || last.privateKey != storedKeyHex || last.smartWallet != storedWallet || last.testnet != "testnet" {
		t.Fatalf("probe did not restore the stored credentials: %+v", last)
	}
}

// TestProbeLiveVenuePredictFunKeepsStoredSecrets covers the browser echo: the
// key comes back masked, so the probe must fall back to the saved one instead of
// signing with "***".
func TestProbeLiveVenuePredictFunKeepsStoredSecrets(t *testing.T) {
	instance, live := newCredentialEngine(t)
	live.applied = nil
	candidate := engine.VenueLiveSettings{
		LiveSettings: engine.LiveSettings{APIKey: "", PrivateKey: netproxy.RedactedSecret},
	}
	if _, err := instance.ProbeLiveVenue(context.Background(), market.VenuePredictFun, candidate, ""); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(live.applied) == 0 {
		t.Fatal("probe did not apply anything")
	}
	if got := live.applied[0]; got.privateKey != storedKeyHex || got.apiKey != "stored-api-key" || got.smartWallet != storedWallet {
		t.Fatalf("masked values must resolve to the stored ones, got %+v", got)
	}
}

// TestProbeLiveVenueRejectsMalformedKey proves a bad key fails the check
// instead of being applied.
func TestProbeLiveVenueRejectsMalformedKey(t *testing.T) {
	instance, live := newCredentialEngine(t)
	live.applied = nil
	candidate := engine.VenueLiveSettings{LiveSettings: engine.LiveSettings{PrivateKey: "0xdeadbeef"}}
	_, err := instance.ProbeLiveVenue(context.Background(), market.VenuePredictFun, candidate, "")
	if err == nil {
		t.Fatal("a malformed private key must fail the probe")
	}
	if !errors.Is(err, engine.ErrInvalidSettings) {
		t.Fatalf("a malformed key must be reported as invalid settings, got %v", err)
	}
	if len(live.applied) != 0 {
		t.Fatalf("a rejected key must never be applied: %+v", live.applied)
	}
}

// TestProbeLiveVenueReadOnlyVenueFails keeps the "not configured" answer honest
// when only the data side is registered.
func TestProbeLiveVenueReadOnlyVenueFails(t *testing.T) {
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, markets: map[string]market.Market{},
	}, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	_, err := instance.ProbeLiveVenue(context.Background(), market.VenuePredictFun, engine.VenueLiveSettings{}, "")
	if err == nil {
		t.Fatal("a venue without a live executor cannot be probed")
	}
}

// TestVenueLiveSettingsAcceptsFunderSpelling covers the API mismatch: the venue
// block embeds the Polymarket credential struct, so `funder` is a field a client
// can send even though a non-Polymarket venue only reads `smart_wallet`. The
// alias keeps the address instead of dropping it on the floor.
func TestVenueLiveSettingsAcceptsFunderSpelling(t *testing.T) {
	fromFunder := engine.VenueLiveSettings{LiveSettings: engine.LiveSettings{Funder: storedWallet}}
	if got := fromFunder.Normalize().SmartWallet; got != storedWallet {
		t.Fatalf("funder must fill an empty smart_wallet, got %q", got)
	}
	both := engine.VenueLiveSettings{
		LiveSettings: engine.LiveSettings{Funder: storedWallet},
		SmartWallet:  otherWallet,
	}
	if got := both.Normalize().SmartWallet; got != otherWallet {
		t.Fatalf("an explicit smart_wallet must win over funder, got %q", got)
	}
	if err := fromFunder.Normalize().Validate(market.VenuePredictFun); err != nil {
		t.Fatalf("a funder-shaped block must validate for Predict.fun: %v", err)
	}
}

// A PUT that does not mention the network must keep the stored one: dropping a
// saved testnet:true would move the next process start onto BNB mainnet.
func TestVenueSystemSettingsMergeKeepsStoredNetwork(t *testing.T) {
	previous := engine.VenueSystemSettings{
		Live: engine.VenueLiveSettings{LiveSettings: engine.LiveSettings{PrivateKey: storedKeyHex, APIKey: "stored-key"}, Testnet: boolPtr(true)},
	}
	incoming := engine.VenueSystemSettings{Live: engine.VenueLiveSettings{SmartWallet: otherWallet}}
	merged := incoming.Merge(previous)
	if merged.Live.Testnet == nil || !*merged.Live.Testnet {
		t.Fatalf("an omitted testnet must inherit the stored value, got %v", merged.Live.Testnet)
	}
	// An explicit value still wins, so the operator can leave testnet.
	explicit := engine.VenueSystemSettings{Live: engine.VenueLiveSettings{Testnet: boolPtr(false)}}
	off := explicit.Merge(previous)
	if off.Live.Testnet == nil || *off.Live.Testnet {
		t.Fatalf("an explicit testnet=false must be honoured, got %v", off.Live.Testnet)
	}
}

// A client that echoes the redacted GET response back must not store the
// placeholder as a credential.
func TestVenueSystemSettingsMergeRestoresRedactedSecrets(t *testing.T) {
	previous := engine.VenueSystemSettings{
		Live: engine.VenueLiveSettings{LiveSettings: engine.LiveSettings{PrivateKey: storedKeyHex, APIKey: "stored-key"}},
	}
	echoed := engine.VenueSystemSettings{
		Live: engine.VenueLiveSettings{LiveSettings: engine.LiveSettings{PrivateKey: netproxy.RedactedSecret}},
	}
	merged := echoed.Merge(previous)
	if merged.Live.PrivateKey != storedKeyHex {
		t.Fatalf("a redacted private key must restore the stored one, got %q", merged.Live.PrivateKey)
	}
	if err := merged.Normalize().Validate(market.VenuePredictFun); err != nil {
		t.Fatalf("a round-tripped redacted block must still validate: %v", err)
	}
}

// boolPtr builds the tri-state network flag used by VenueLiveSettings.
func boolPtr(v bool) *bool { return &v }
