package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/venue"
)

// benchRepository is a bounded in-memory Repository for the scan benchmarks.
// It embeds the interface so only the methods the scan path actually uses are
// implemented; any other call panics instead of silently returning fabricated
// data, which keeps the fixture honest when the scan path changes.
// The existing engine tests live in package engine_test and build on
// pennyharvester/backend/internal/store. An internal test file in package engine
// cannot import store without an import cycle (store imports engine), so these
// benchmarks use the bounded in-memory repository below instead of
// newTestEngineWithRegistry; the store benchmarks measure real SQLite I/O.
type benchRepository struct {
	Repository
	accounts  []Account
	settings  Settings
	snapshots map[int64]EquityPoint
	snapCalls int
}

func (r *benchRepository) LoadSettings(context.Context) (Settings, error) { return r.settings, nil }

func (r *benchRepository) LoadSystemSettings(context.Context) (SystemSettings, error) {
	return SystemSettings{}, nil
}

func (r *benchRepository) LoadAccountSettings(context.Context, int64) (Settings, error) {
	return r.settings, nil
}

func (r *benchRepository) ListAccounts(context.Context) ([]Account, error) {
	return r.accounts, nil
}

func (r *benchRepository) AccountByID(_ context.Context, id int64) (Account, error) {
	for _, account := range r.accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return Account{}, ErrAccountNotFound
}

func (r *benchRepository) ListPositions(context.Context, int64, string, market.Venue) ([]Position, error) {
	return nil, nil
}

func (r *benchRepository) ListUnresolvedOrders(context.Context, int64) ([]Order, error) {
	return nil, nil
}

func (r *benchRepository) ListUnresolvedChainOperations(context.Context, int) ([]ChainOperation, error) {
	return nil, nil
}

func (r *benchRepository) EarliestAutoBuyAt(context.Context, int64) (*time.Time, error) {
	return nil, nil
}

func (r *benchRepository) CountOrders(context.Context, int64, string, market.Venue) (int64, error) {
	return 0, nil
}

func (r *benchRepository) ListSnapshots(context.Context, int64, int) ([]EquityPoint, error) {
	return nil, nil
}

func (r *benchRepository) AddSnapshot(_ context.Context, accountID int64, point EquityPoint) error {
	// A scan appends one snapshot per account per iteration. Keeping only the
	// newest point keeps the fake repository bounded; SQLite persistence is
	// measured by the store benchmarks instead.
	r.snapshots[accountID] = point
	r.snapCalls++
	return nil
}

// benchProvider serves a fixed market list and never touches the network. The
// error field models a venue outage for the failure-path benchmark.
type benchProvider struct {
	markets []market.Market
	listErr error
}

func (p *benchProvider) Name() string        { return "benchmark" }
func (p *benchProvider) Venue() market.Venue { return market.VenuePolymarket }
func (p *benchProvider) ListOpen(context.Context, int) ([]market.Market, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.markets, nil
}

func (p *benchProvider) Get(_ context.Context, id string) (market.Market, error) {
	return market.Market{}, fmt.Errorf("benchmark market %s not found", id)
}

// benchmarkMarkets builds a deterministic set of binary tail markets that pass
// the default strategy gates: price 0.960–0.989, liquidity >= 20,000 and an end
// date between 2 and 21 days out.
func benchmarkMarkets(count int) []market.Market {
	now := time.Now().UTC()
	markets := make([]market.Market, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("bench-market-%03d", i)
		price := 0.96 + float64(i%30)*0.001
		markets = append(markets, market.Market{
			ID: id, ConditionID: "condition-" + id, Question: "Benchmark market " + id,
			Slug: id, Active: true, EndDate: now.AddDate(0, 0, 2+i%20),
			Liquidity: 20000 + float64(i%50)*500, Volume: 50000,
			Outcomes: []market.Outcome{
				{Name: "Yes", TokenID: id + "-yes", Price: price},
				{Name: "No", TokenID: id + "-no", Price: 1 - price},
			},
		})
	}
	return markets
}

func newBenchmarkEngine(b *testing.B, paperAccounts int, provider *benchProvider) (*Engine, *benchRepository) {
	b.Helper()
	settings := DefaultSettings()
	settings.Venues = map[market.Venue]VenueSettings{market.VenuePolymarket: {Enabled: true}}

	accounts := make([]Account, 0, paperAccounts)
	for i := 1; i <= paperAccounts; i++ {
		accounts = append(accounts, Account{
			ID: int64(i), Name: fmt.Sprintf("Benchmark Paper %d", i), Mode: AccountModePaper,
			Venue: market.VenuePolymarket, BaselineEquity: settings.InitialBankroll,
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	repository := &benchRepository{
		accounts: accounts, settings: settings,
		snapshots: make(map[int64]EquityPoint, paperAccounts),
	}

	registry := venue.NewRegistry()
	caps := venue.Capabilities{
		Venue: market.VenuePolymarket, Label: market.VenuePolymarket.Label(),
		Currency: market.CurrencyUSDC, SupportsSettlement: true,
	}
	if err := registry.Register(caps, provider, nil); err != nil {
		b.Fatalf("register benchmark venue: %v", err)
	}
	instance, err := New(context.Background(), repository, registry, nil)
	if err != nil {
		b.Fatalf("create benchmark engine: %v", err)
	}
	return instance, repository
}

func BenchmarkScanManyMarkets(b *testing.B) {
	const markets = 200
	instance, repository := newBenchmarkEngine(b, 1, &benchProvider{markets: benchmarkMarkets(markets)})
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := instance.ScanOnly(ctx); err != nil {
			b.Fatalf("scan %d: %v", i, err)
		}
	}
	b.StopTimer()

	if got := len(instance.OpportunitiesFor(1).Items); got != markets {
		b.Fatalf("expected %d opportunities for the single account, got %d", markets, got)
	}
	if repository.snapCalls != b.N {
		b.Fatalf("expected one snapshot per scan, got %d for %d scans", repository.snapCalls, b.N)
	}
}

func BenchmarkScanTwoAccounts(b *testing.B) {
	const markets = 200
	instance, repository := newBenchmarkEngine(b, 2, &benchProvider{markets: benchmarkMarkets(markets)})
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := instance.ScanOnly(ctx); err != nil {
			b.Fatalf("scan %d: %v", i, err)
		}
	}
	b.StopTimer()

	for accountID := int64(1); accountID <= 2; accountID++ {
		if got := len(instance.OpportunitiesFor(accountID).Items); got != markets {
			b.Fatalf("expected %d opportunities for account %d, got %d", markets, accountID, got)
		}
	}
	if repository.snapCalls != 2*b.N {
		b.Fatalf("expected two snapshots per scan, got %d for %d scans", repository.snapCalls, b.N)
	}
}

func BenchmarkScanVenueFailure(b *testing.B) {
	instance, repository := newBenchmarkEngine(b, 1, &benchProvider{listErr: errors.New("benchmark venue unavailable")})
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := instance.ScanOnly(ctx)
		if !errors.Is(err, ErrUpstream) {
			b.Fatalf("scan %d: expected ErrUpstream, got %v", i, err)
		}
	}
	b.StopTimer()

	// Account maintenance still runs when discovery fails, so the benchmark
	// covers the failure path rather than short-circuiting before it.
	if repository.snapCalls != b.N {
		b.Fatalf("expected one snapshot per failed scan, got %d for %d scans", repository.snapCalls, b.N)
	}
}
