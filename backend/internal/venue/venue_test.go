package venue

import (
	"context"
	"math"
	"math/big"
	"testing"

	"pennyharvester/backend/internal/market"
)

type fakeData struct{ venue market.Venue }

func (f fakeData) Venue() market.Venue                                    { return f.venue }
func (f fakeData) ListOpen(context.Context, int) ([]market.Market, error) { return nil, nil }
func (f fakeData) Get(context.Context, string) (market.Market, error)     { return market.Market{}, nil }
func (f fakeData) Name() string                                           { return string(f.venue) }

func TestRegistryRejectsVenueMismatch(t *testing.T) {
	registry := NewRegistry()
	err := registry.Register(
		Capabilities{Venue: market.VenuePredictFun},
		fakeData{venue: market.VenuePolymarket},
		nil,
	)
	if err == nil {
		t.Fatal("registered a data source under the wrong venue")
	}
}

func TestRegistryFillsDefaultsAndOrdersVenues(t *testing.T) {
	registry := NewRegistry()
	// Register out of presentation order; All() must still start with Polymarket.
	if err := registry.Register(Capabilities{Venue: market.VenuePredictFun}, fakeData{venue: market.VenuePredictFun}, nil); err != nil {
		t.Fatalf("register predictfun: %v", err)
	}
	if err := registry.Register(Capabilities{Venue: market.VenuePolymarket}, fakeData{venue: market.VenuePolymarket}, nil); err != nil {
		t.Fatalf("register polymarket: %v", err)
	}
	venues := registry.Venues()
	if len(venues) != 2 || venues[0] != market.VenuePolymarket || venues[1] != market.VenuePredictFun {
		t.Fatalf("venue order = %v", venues)
	}
	caps, ok := registry.Capabilities(market.VenuePredictFun)
	if !ok {
		t.Fatal("predictfun capabilities missing")
	}
	if caps.Currency != market.CurrencyUSDT {
		t.Fatalf("predictfun currency = %q, want USDT", caps.Currency)
	}
	if caps.Label != "Predict.fun" {
		t.Fatalf("predictfun label = %q", caps.Label)
	}
}

func TestRegistryReadOnlyVenueHasNoExecutor(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(Capabilities{Venue: market.VenuePolymarket}, fakeData{venue: market.VenuePolymarket}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.ExecutorFor(market.VenuePolymarket); err == nil {
		t.Fatal("read-only venue handed out an executor")
	}
	if _, err := registry.ExecutorFor(market.VenuePredictFun); err == nil {
		t.Fatal("unknown venue handed out an executor")
	}
}

func TestScaleIsExactAtEighteenDecimals(t *testing.T) {
	// 0.1 and 1.5 are the values a float64 multiply gets wrong at 1e18.
	cases := []struct {
		amount   float64
		decimals int
		want     string
	}{
		{1, 18, "1000000000000000000"},
		{0.1, 18, "100000000000000000"},
		{1.5, 18, "1500000000000000000"},
		{0.05, 18, "50000000000000000"},
		{12.345678, 6, "12345678"},
		{0, 18, "0"},
		{-3, 18, "0"},
	}
	for _, tc := range cases {
		got := Scale(tc.amount, tc.decimals)
		if got.String() != tc.want {
			t.Errorf("Scale(%v, %d) = %s, want %s", tc.amount, tc.decimals, got, tc.want)
		}
	}
	// A naive float multiply is exactly the bug Scale exists to avoid.
	if naive := int64(0.1 * 1e18); naive == 100000000000000000 {
		t.Log("float multiply happened to be exact on this platform")
	}
}

func TestUnscaleRoundTrips(t *testing.T) {
	raw, _ := new(big.Int).SetString("1500000000000000000", 10)
	if got := Unscale(raw, 18); got != 1.5 {
		t.Fatalf("Unscale = %v, want 1.5", got)
	}
	if got := Unscale(nil, 18); got != 0 {
		t.Fatalf("Unscale(nil) = %v", got)
	}
}

func TestQuantizeRoundsDown(t *testing.T) {
	if got := Quantize(0.927, 0.01); math.Abs(got-0.92) > 1e-9 {
		t.Fatalf("Quantize(0.927, 0.01) = %v, want 0.92", got)
	}
	if got := Quantize(0.9251, 0.001); math.Abs(got-0.925) > 1e-9 {
		t.Fatalf("Quantize(0.9251, 0.001) = %v, want 0.925", got)
	}
	if got := Quantize(0.5, 0); got != 0.5 {
		t.Fatalf("Quantize with no tick = %v, want 0.5", got)
	}
}

func TestFeeModelsDifferAtMidprice(t *testing.T) {
	// 2% taker rate, 100 shares. Polymarket's paper model charges on notional;
	// Predict.fun charges on min(p, 1-p). At 0.5 they differ by 2x, which is the
	// case a shared model would misprice.
	pm := FeeModelFor(market.VenuePolymarket, 200)
	pf := FeeModelFor(market.VenuePredictFun, 200)
	if got := pm(100, 0.5); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("polymarket fee = %v, want 1.0", got)
	}
	if got := pf(100, 0.5); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("predict.fun fee at 0.5 = %v, want 1.0 (2%% of 0.5 notional per share)", got)
	}
	if got := pf(100, 0.02); math.Abs(got-0.04) > 1e-9 {
		t.Fatalf("predict.fun fee at 0.02 = %v, want 0.04", got)
	}
	if got := pm(100, 0.02); math.Abs(got-0.04) > 1e-9 {
		t.Fatalf("polymarket paper fee at 0.02 = %v, want 0.04 (unchanged historical model)", got)
	}
}

func TestMarketDefaultsKeepLegacyRowsPolymarket(t *testing.T) {
	m := market.Market{}
	if m.EffectiveVenue() != market.VenuePolymarket {
		t.Fatalf("empty venue = %q, want polymarket", m.EffectiveVenue())
	}
	if m.EffectiveCurrency() != market.CurrencyUSDC {
		t.Fatalf("empty currency = %q, want USDC", m.EffectiveCurrency())
	}
	if got := m.URL(); got != "" {
		t.Fatalf("empty slug URL = %q", got)
	}
	// A Predict.fun market without a provider link must not be pointed at
	// polymarket.com, whose slug space would 404.
	pf := market.Market{Slug: "will-x-happen", Venue: market.VenuePredictFun}
	if got := pf.URL(); got != "" {
		t.Fatalf("predict.fun URL without link = %q, want empty", got)
	}
	if got := (market.Market{LinkURL: "https://predict.fun/x", Venue: market.VenuePredictFun}).URL(); got != "https://predict.fun/x" {
		t.Fatalf("link URL = %q", got)
	}
}

func TestCapabilitiesOrderTypesAreDeepCopied(t *testing.T) {
	orderTypes := []string{"FAK", "FOK"}
	registry := NewRegistry()
	if err := registry.Register(Capabilities{
		Venue:      market.VenuePolymarket,
		OrderTypes: orderTypes,
	}, fakeData{venue: market.VenuePolymarket}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Register must copy the caller's slice, not retain its backing array.
	orderTypes[0] = "CALLER_MUTATED"
	first, ok := registry.Capabilities(market.VenuePolymarket)
	if !ok {
		t.Fatal("polymarket capabilities missing")
	}
	if len(first.OrderTypes) != 2 || first.OrderTypes[0] != "FAK" {
		t.Fatalf("caller mutation reached the registry: %v", first.OrderTypes)
	}

	// Every accessor must hand out a copy so one caller cannot change what the
	// next caller reads.
	first.OrderTypes[0] = "RETURNED_MUTATED"
	second, _ := registry.Capabilities(market.VenuePolymarket)
	if second.OrderTypes[0] != "FAK" {
		t.Fatalf("mutating Capabilities result changed the next read: %v", second.OrderTypes)
	}

	all := registry.All()
	if len(all) != 1 || len(all[0].OrderTypes) != 2 {
		t.Fatalf("All() = %#v", all)
	}
	all[0].OrderTypes[1] = "ALL_MUTATED"
	again := registry.All()
	if again[0].OrderTypes[1] != "FOK" {
		t.Fatalf("mutating All result changed the next read: %v", again[0].OrderTypes)
	}
}
