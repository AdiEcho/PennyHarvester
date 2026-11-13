package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/venue"
)

const (
	localizationTimeout    = 2 * time.Second
	positionRefreshTimeout = 3 * time.Second
	// positionBatchTimeout bounds the one batched read that resolves every
	// position market the scan view did not carry. Marking runs even when the
	// market-list budget is already spent, because a stale mark must not hide
	// behind a slow radar refresh.
	positionBatchTimeout = 8 * time.Second
	// liveBalanceTimeout bounds the wallet read a dashboard request makes against
	// the venue. The dashboard is polled by the web UI, so an unreachable venue
	// must report stale cash instead of holding the request open until the HTTP
	// client gives up.
	liveBalanceTimeout = 5 * time.Second
	// ledgerWriteTimeout bounds the final database writes of a scan. The market
	// requests may be cancelled by an HTTP client, but a settlement or mark that
	// was already computed must still reach the ledger.
	ledgerWriteTimeout = 15 * time.Second
)

type Engine struct {
	repo   Repository
	venues *venue.Registry
	proxy  *netproxy.Manager

	tradeMu     sync.Mutex
	mu          sync.RWMutex
	settings    Settings
	system      SystemSettings
	systemError string
	// walletCache remembers successful wallet discoveries and verified proxy
	// funder saves for a short time. It has its own lock so an admin save can
	// check it without touching the settings lock, and it is initialized on the
	// first insert so zero-value Engine values used by tests still work.
	walletCacheMu sync.Mutex
	walletCache   map[string]walletCacheEntry
	opportunities []Opportunity
	// opportunitiesByAccount holds each account's own radar view, filtered by
	// that account's strategy. The union above keeps legacy callers and token
	// lookups working across accounts.
	opportunitiesByAccount map[int64][]Opportunity
	// scannedVenues records which venues contributed market data to the last
	// scan, including venues that returned markets but no qualifying rows.
	scannedVenues []market.Venue
	lastScanAt    *time.Time
	lastSyncAt    *time.Time
	lastError     string
	scanCount     int64
	running       bool

	startOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	// scanReset wakes the scheduler after a settings change so a shorter
	// interval starts now instead of after the old, longer sleep.
	scanReset chan struct{}
}

func New(ctx context.Context, repo Repository, venues *venue.Registry, proxy *netproxy.Manager) (*Engine, error) {
	settings, err := repo.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	system, err := repo.LoadSystemSettings(ctx)
	if err != nil {
		return nil, err
	}
	// A fresh database returns the built-in defaults without normalizing them;
	// normalize once here so venues.polymarket always has a readable view.
	system = system.Normalize()
	if proxy == nil {
		proxy = netproxy.NewMust(system.Proxy)
	} else if err := proxy.Apply(system.Proxy); err != nil {
		// A row written by an older build must not stop the process from
		// booting; the system settings page will surface the stored value.
		proxy = netproxy.NewMust(netproxy.DefaultConfig())
	}
	// Paper accounts created before the baseline was persisted (or legacy
	// accounts) get the current default once. After that the stored baseline
	// is authoritative and changing the global default does not rewrite
	// historical performance.
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, account := range accounts {
		if account.Mode == AccountModePaper && account.BaselineEquity <= 0 {
			if err := repo.UpdateAccountBaseline(ctx, account.ID, settings.InitialBankroll); err != nil {
				return nil, err
			}
		}
	}
	if venues == nil {
		venues = venue.NewRegistry()
	}
	instance := &Engine{
		repo: repo, venues: venues, proxy: proxy, settings: settings, system: system,
		opportunities: []Opportunity{}, opportunitiesByAccount: map[int64][]Opportunity{},
		running: true, done: make(chan struct{}), scanReset: make(chan struct{}, 1),
	}
	// Apply web-managed runtime settings on top of the process environment. A
	// row written by an older build has empty fields and therefore inherits the
	// startup configuration. An unusable row must not prevent boot; it is
	// surfaced through SystemRuntime instead.
	if err := instance.applyRuntimeSettings(system); err != nil {
		instance.systemError = err.Error()
	}
	return instance, nil
}

// Venues lists the registered venues in presentation order.
func (e *Engine) Venues() []market.Venue {
	if e.venues == nil {
		return nil
	}
	return e.venues.Venues()
}

// VenueCapabilities returns every registered venue's fact sheet for the API.
func (e *Engine) VenueCapabilities() []venue.Capabilities {
	if e.venues == nil {
		return []venue.Capabilities{}
	}
	return e.venues.All()
}

// data returns a venue's market data source. A nil result means the venue is
// not registered, which callers must treat as unavailable rather than falling
// back to another venue.
func (e *Engine) data(v market.Venue) venue.MarketData {
	if e.venues == nil {
		return nil
	}
	return e.venues.Data(effectiveVenue(v))
}

// executor resolves a venue's trading surface. The error distinguishes a
// read-only venue from one that is merely unconfigured.
func (e *Engine) executor(v market.Venue) (venue.Executor, error) {
	if e.venues == nil {
		return nil, fmt.Errorf("%w: %s", ErrVenueReadOnly, effectiveVenue(v).Label())
	}
	exec, err := e.venues.ExecutorFor(effectiveVenue(v))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVenueReadOnly, err)
	}
	return exec, nil
}

func (e *Engine) capabilities(v market.Venue) (venue.Capabilities, bool) {
	if e.venues == nil {
		return venue.Capabilities{}, false
	}
	return e.venues.Capabilities(effectiveVenue(v))
}

// supportsSettlement reports whether the engine may settle this venue's
// positions from reference data. Polymarket resolves through its own API;
// Predict.fun publishes the winning outcome, which the provider maps onto a 1/0
// payout. A venue without such a feed keeps its positions open instead of being
// guessed closed from another exchange's prices.
func (e *Engine) supportsSettlement(v market.Venue) bool {
	caps, ok := e.capabilities(v)
	if !ok {
		return effectiveVenue(v) == market.VenuePolymarket
	}
	return caps.SupportsSettlement
}

// dataHealth reports each venue's most recent scan quality without triggering a
// scan. Providers that cannot describe their data are omitted rather than
// reported as healthy.
func (e *Engine) dataHealth() map[market.Venue]market.DataHealth {
	health := make(map[market.Venue]market.DataHealth)
	for _, v := range e.Venues() {
		if provider, ok := e.data(v).(market.HealthProvider); ok {
			health[v] = provider.Health()
		}
	}
	if len(health) == 0 {
		return nil
	}
	return health
}

// sourceName is the aggregate label the radar and dashboard render. With one
// registered venue it is exactly that provider's name, which keeps the
// Polymarket label byte-identical.
func (e *Engine) sourceName() string {
	names := make([]string, 0, len(e.Venues()))
	for _, v := range e.Venues() {
		if data := e.data(v); data != nil {
			names = append(names, data.Name())
		}
	}
	return strings.Join(names, " + ")
}

// marketKey namespaces an identifier by venue. Token and market ids are only
// unique inside one venue: both exchanges use decimal ERC-1155 ids, so a bare
// string key would let a Predict.fun row mask a Polymarket row.
func marketKey(v market.Venue, id string) string {
	return string(effectiveVenue(v)) + "\x00" + id
}

// eventKey groups the markets that settle together. The venue prefix is
// required because an event id is only meaningful inside its own venue. An
// empty key means the market carries no event, and such rows are capped by
// market alone.
func eventKey(v market.Venue, eventID string) string {
	if eventID == "" {
		return ""
	}
	return string(effectiveVenue(v)) + "\x00" + eventID
}

// venueList returns the venues a scan should visit for the given options.
func (e *Engine) venueList(settings Settings, only market.Venue) []market.Venue {
	if only != "" && !only.Valid() {
		return nil
	}
	venues := make([]market.Venue, 0, len(e.Venues()))
	for _, v := range e.Venues() {
		if only != "" && v != only {
			continue
		}
		if !settings.For(v).Enabled {
			continue
		}
		venues = append(venues, v)
	}
	return venues
}

func (e *Engine) Start(parent context.Context) {
	e.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		e.cancel = cancel
		go e.run(ctx)
	})
}

func (e *Engine) Stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
	}
}

func (e *Engine) run(ctx context.Context) {
	defer close(e.done)
	for {
		started := time.Now()
		settings := e.Settings()
		interval := time.Duration(settings.ScanIntervalSeconds) * time.Second
		if e.isRunning() {
			scanCtx, cancel := context.WithTimeout(ctx, scanBudget(interval))
			_ = e.Scan(scanCtx)
			cancel()
		}
		elapsed := time.Since(started)
		wait := waitForNextScan(interval, elapsed)
		if elapsed > interval {
			log.Printf("扫描耗时 %s，超过 %s 的扫描间隔；下一轮在 %s 后开始", elapsed.Round(time.Millisecond), interval, wait.Round(time.Millisecond))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-e.scanReset:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (e *Engine) SystemSettings() SystemSettings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	system := e.system
	system.Venues = cloneVenueSystemSettings(system.Venues)
	return system
}

// cloneVenueSystemSettings copies the per-venue credential blocks so a returned
// snapshot cannot be edited in place behind the engine's back.
func cloneVenueSystemSettings(in map[market.Venue]VenueSystemSettings) map[market.Venue]VenueSystemSettings {
	if in == nil {
		return nil
	}
	out := make(map[market.Venue]VenueSystemSettings, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// SystemRuntime reports the effective endpoints and every venue's executor
// state. The flat Gamma/CLOB fields remain the Polymarket view.
func (e *Engine) SystemRuntime() SystemRuntime {
	runtime := SystemRuntime{}
	if provider, ok := e.data(market.VenuePolymarket).(dataSourceRuntime); ok && provider != nil {
		runtime.GammaBaseURL = provider.BaseURL()
		runtime.GammaDefault = provider.DefaultBaseURL()
	}
	if live, ok := e.venues.Executor(market.VenuePolymarket).(liveSettingsRuntime); ok && live != nil {
		runtime.CLOBBaseURL = live.EffectiveCLOBURL()
		runtime.LiveEnvironment = live.Environment()
		runtime.CLOBDefault = runtime.LiveEnvironment.CLOBBaseURL
	}
	runtime.LiveConfigured, runtime.LiveReady, runtime.LiveError = e.LiveStatus()
	runtime.Venues = e.venueRuntimes()
	e.mu.RLock()
	runtime.Error = e.systemError
	e.mu.RUnlock()
	// A venue without settlement support must say so where an operator can see
	// it, instead of leaving positions open with no explanation.
	var notices []string
	for _, v := range e.Venues() {
		if caps, ok := e.capabilities(v); ok && !caps.SupportsSettlement {
			notices = append(notices, fmt.Sprintf("%s 暂不支持自动结算", v.Label()))
		}
	}
	if notice := strings.Join(notices, "; "); notice != "" {
		if runtime.Error != "" {
			runtime.Error += "; " + notice
		} else {
			runtime.Error = notice
		}
	}
	return runtime
}

// venueNetworkReporter is implemented by executors that can say which network
// they are trading on right now.
type venueNetworkReporter interface {
	Testnet() bool
}

// venueRuntimes builds one status row per registered venue.
func (e *Engine) venueRuntimes() []VenueRuntime {
	runtimes := make([]VenueRuntime, 0, len(e.Venues()))
	for _, v := range e.Venues() {
		status := VenueRuntime{Venue: v}
		if caps, ok := e.capabilities(v); ok {
			status.Testnet = caps.Testnet
		}
		exec := e.venues.Executor(v)
		// Prefer the network the executor is actually pointed at: capabilities
		// are frozen at registration, so a credential save that switches networks
		// would otherwise leave the badge lying about which chain is live.
		if reporter, ok := exec.(venueNetworkReporter); ok {
			status.Testnet = reporter.Testnet()
		}
		if exec == nil {
			status.Error = ErrVenueReadOnly.Error()
			runtimes = append(runtimes, status)
			continue
		}
		status.Configured = exec.Configured()
		status.Signer = exec.Signer()
		status.Maker = exec.Maker()
		status.Error = exec.ReadyError()
		status.Ready = status.Configured && status.Error == ""
		runtimes = append(runtimes, status)
	}
	return runtimes
}

func (e *Engine) Status() EngineStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return EngineStatus{Running: e.running, LastScanAt: e.lastScanAt, LastError: e.lastError, ScanCount: e.scanCount}
}

func (e *Engine) statusFor(mode AccountMode) EngineStatus {
	status := e.Status()
	status.Mode = string(mode)
	return status
}

func (e *Engine) SetRunning(running bool) EngineStatus {
	e.mu.Lock()
	e.running = running
	e.mu.Unlock()
	return e.Status()
}

func (e *Engine) isRunning() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.running
}

// LiveStatus reports the Polymarket executor state independently of the
// currently selected default account. It is the legacy health-check shape;
// SystemRuntime carries the per-venue view.
func (e *Engine) LiveStatus() (configured, ready bool, message string) {
	return e.LiveStatusFor(market.VenuePolymarket)
}

// LiveStatusFor reports one venue's executor state.
func (e *Engine) LiveStatusFor(v market.Venue) (configured, ready bool, message string) {
	exec, err := e.executor(v)
	if err != nil {
		return false, false, ErrVenueReadOnly.Error()
	}
	configured = exec.Configured()
	message = exec.ReadyError()
	return configured, configured && message == "", message
}
