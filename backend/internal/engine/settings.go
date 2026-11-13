package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/venue"
)

func (e *Engine) Settings() Settings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	settings := e.settings
	// The venue map is shared state: a caller that mutates what it loaded would
	// change the engine's configuration without saving it.
	settings.Venues = cloneVenueSettings(settings.Venues)
	return settings
}

// cloneVenueSettings copies the per-venue strategy blocks so a returned snapshot
// is safe to edit and save back.
func cloneVenueSettings(in map[market.Venue]VenueSettings) map[market.Venue]VenueSettings {
	if in == nil {
		return nil
	}
	out := make(map[market.Venue]VenueSettings, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// normalizeSettingsInput fills the server-owned version and the fields older
// API clients omit, so a partial payload is not rejected outright.
func normalizeSettingsInput(settings Settings) Settings {
	// The version is owned by the server so a client payload can never roll the
	// one-time fee migration back or trigger it again.
	settings.FeeModelVersion = FeeModelVersion
	defaults := DefaultSettings()
	if settings.DefaultCategory == "" {
		settings.DefaultCategory = defaults.DefaultCategory
	}
	if settings.SportsPolicy == "" {
		settings.SportsPolicy = defaults.SportsPolicy
	}
	if settings.StopLossRatio == 0 {
		settings.StopLossRatio = defaults.StopLossRatio
	}
	return settings
}

// mergeVenueSettings keeps per-venue overrides a payload does not mention. An
// old client that only sends the flat fields must not erase the venue map.
func mergeVenueSettings(previous, settings Settings) Settings {
	if settings.Venues == nil {
		settings.Venues = previous.Venues
		return settings
	}
	if len(previous.Venues) == 0 {
		return settings
	}
	merged := make(map[market.Venue]VenueSettings, len(previous.Venues)+len(settings.Venues))
	for v, entry := range previous.Venues {
		merged[v] = entry
	}
	for v, entry := range settings.Venues {
		merged[v] = entry
	}
	settings.Venues = merged
	return settings
}

func (e *Engine) UpdateSettings(ctx context.Context, settings Settings) error {
	settings = normalizeSettingsInput(settings)
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	settings = mergeVenueSettings(e.Settings(), settings)
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := e.validateStopLossExit(settings); err != nil {
		return err
	}
	if err := e.repo.SaveSettings(ctx, settings); err != nil {
		return err
	}
	e.mu.Lock()
	e.settings = settings
	e.mu.Unlock()
	// A shorter cadence should apply before the old sleep expires.
	select {
	case e.scanReset <- struct{}{}:
	default:
	}
	return nil
}

// SettingsFor returns the strategy block that applies to one account. A
// non-positive account ID resolves to the global defaults, which is also the
// template a new account inherits until it saves its own strategy.
func (e *Engine) SettingsFor(ctx context.Context, accountID int64) (Settings, error) {
	return e.settingsForAccount(ctx, accountID)
}

func (e *Engine) settingsForAccount(ctx context.Context, accountID int64) (Settings, error) {
	if accountID <= 0 {
		return e.Settings(), nil
	}
	settings, err := e.repo.LoadAccountSettings(ctx, accountID)
	if err != nil {
		return Settings{}, err
	}
	// Scanning is a process-wide scheduler, not account state. Accounts may
	// still carry a scan interval from older builds, but the value the engine
	// actually uses is the global one. Returning the effective value keeps the
	// settings page from showing a number that has no effect.
	settings.ScanIntervalSeconds = e.Settings().ScanIntervalSeconds
	return settings, nil
}

// UpdateAccountSettings validates and persists one account's strategy block.
// The global settings stay untouched: saving account A must never change what
// account B trades.
func (e *Engine) UpdateAccountSettings(ctx context.Context, accountID int64, settings Settings) error {
	if accountID <= 0 {
		return fmt.Errorf("%w: account_id 不能为空", ErrInvalidOrder)
	}
	if _, err := e.repo.AccountByID(ctx, accountID); err != nil {
		return err
	}
	settings = normalizeSettingsInput(settings)
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	previous, err := e.settingsForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	settings = mergeVenueSettings(previous, settings)
	// The scan interval is global. Keep the account row consistent with the
	// scheduler so a later read cannot surface a stale per-account value.
	settings.ScanIntervalSeconds = previous.ScanIntervalSeconds
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := e.validateStopLossExit(settings); err != nil {
		return err
	}
	return e.repo.SaveAccountSettings(ctx, accountID, settings)
}

// validateStopLossExit refuses to enable the stop-loss when this build could not
// exit what the settings would buy. Every venue floors a sell by shares while a
// buy is sized in money, and one share always costs less than one currency unit
// (prices are strictly below 1), so a single order below the venue's minimum
// share count always lands under that floor. Such a position is not lost: it
// simply cannot be sold before settlement, which is the opposite of what a
// stop-loss promises. The rule lives here rather than in Settings.Validate
// because persisted settings are validated on load, and an installation that
// already runs the stop-loss on small orders must keep loading.
func (e *Engine) validateStopLossExit(settings Settings) error {
	if !settings.StopLossEnabled {
		return nil
	}
	for _, v := range market.VenueOrder {
		effective := settings.For(v)
		if !effective.Enabled {
			continue
		}
		caps, ok := e.capabilities(v)
		if !ok || caps.MinSellShares <= 0 {
			continue
		}
		if effective.OrderSize+1e-9 >= caps.MinSellShares {
			continue
		}
		return fmt.Errorf("%w: 开启自动止损需要 %s 的单笔金额不低于 %.0f %s（该场所卖单最小 %.0f 份，单笔金额低于这个数就买不到可卖出的份额，仓位只能持有到结算）",
			ErrInvalidSettings, caps.Label, caps.MinSellShares, caps.Currency, caps.MinSellShares)
	}
	return nil
}

// PatchSystemSettings reads, mutates, validates, applies and persists the
// system configuration under one lock. Callers that submit a partial patch use
// this instead of reading a snapshot and writing it back, so two independent
// updates cannot overwrite each other with a stale copy.
func (e *Engine) PatchSystemSettings(ctx context.Context, mutate func(*SystemSettings) error) (SystemSettings, error) {
	// Serialize with scans and order placement so a live order cannot be
	// signed with one credential set while the client is being rebuilt for
	// another.
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	previous := e.SystemSettings()
	candidate := previous
	if err := mutate(&candidate); err != nil {
		return previous, err
	}
	candidate = candidate.Normalize()
	if err := candidate.Validate(); err != nil {
		return previous, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	// Validate the exact candidate before any runtime object is switched. This
	// runs under tradeMu because a proxy wallet only counts after discovery
	// confirmed its owner; unchanged verified wallets hit the in-memory cache,
	// and this is an admin-save path rather than an order path, so the network
	// round trip is acceptable. The discovery client keeps its own timeout.
	if err := e.verifyProxyFunderForSave(ctx, candidate); err != nil {
		return previous, err
	}
	if err := e.applyRuntimeSettings(candidate); err != nil {
		// A candidate can fail after the first executor was already switched;
		// restore every applied object before reporting the rejected patch.
		if rollbackErr := e.applyRuntimeSettings(previous); rollbackErr != nil {
			e.mu.Lock()
			e.systemError = fmt.Sprintf("配置应用失败且回滚失败，交易已阻断：%v", rollbackErr)
			e.mu.Unlock()
			return previous, fmt.Errorf("%w: 配置应用失败且回滚失败: %v", ErrInvalidSettings, rollbackErr)
		}
		e.mu.Lock()
		e.systemError = ""
		e.mu.Unlock()
		return previous, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	if err := e.repo.SaveSystemSettings(ctx, candidate); err != nil {
		if rollbackErr := e.applyRuntimeSettings(previous); rollbackErr != nil {
			e.mu.Lock()
			e.systemError = fmt.Sprintf("保存配置失败且回滚失败，交易已阻断：%v", rollbackErr)
			e.mu.Unlock()
			return previous, fmt.Errorf("%w: 保存配置失败且回滚失败: %v", ErrInvalidSettings, rollbackErr)
		}
		return previous, err
	}
	e.mu.Lock()
	e.system = candidate
	e.systemError = ""
	e.mu.Unlock()
	for _, v := range e.Venues() {
		if reloader, ok := e.venues.Executor(v).(interface{ ReloadProxy() }); ok && reloader != nil {
			reloader.ReloadProxy()
		}
	}
	return candidate, nil
}

// UpdateSystemSettings persists the runtime egress, endpoint and live-trading
// configuration and applies it without a restart. Values are applied before
// the row is saved so an unusable URL or credential set never lands in the
// database; a failed save rolls the live clients back to the previous state.
func (e *Engine) UpdateSystemSettings(ctx context.Context, settings SystemSettings) error {
	_, err := e.PatchSystemSettings(ctx, func(candidate *SystemSettings) error {
		*candidate = settings
		return nil
	})
	return err
}

// ProbeLive checks candidate live credentials against the venue's trading API
// without saving them or placing an order. The browser echoes masked secrets
// back, so they are restored from the stored settings first. Only the
// Polymarket adapter implements the probe interface today.
func (e *Engine) ProbeLive(ctx context.Context, v market.Venue, settings LiveSettings, clobBaseURL string) (LiveProbe, error) {
	return e.ProbeLiveVenue(ctx, v, VenueLiveSettings{LiveSettings: settings}, clobBaseURL)
}

// ProbeLiveVenue is the venue-aware credential check used by the settings page.
// Polymarket keeps its dedicated /auth probe; Predict.fun has no such endpoint,
// so its credentials are validated locally and a collateral read through the
// BNB chain proves the key and the RPC endpoint agree.
func (e *Engine) ProbeLiveVenue(ctx context.Context, v market.Venue, settings VenueLiveSettings, clobBaseURL string) (LiveProbe, error) {
	v = effectiveVenue(v)
	if v == market.VenuePredictFun {
		return e.probePredictFunLive(ctx, v, settings, clobBaseURL)
	}
	flat := settings.LiveSettings.MergeSecret(e.SystemSettings().Live).Normalize()
	if err := flat.Validate(); err != nil {
		return LiveProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	exec, err := e.executor(v)
	if err != nil {
		return LiveProbe{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	probe, ok := exec.(liveProbeRuntime)
	if !ok || probe == nil {
		return LiveProbe{}, fmt.Errorf("%w: %s 暂不支持凭据检测", ErrInvalidSettings, v.Label())
	}
	return probe.Probe(ctx, flat, clobBaseURL)
}

// ProbeRelayer checks a Relayer API key without submitting anything. Relayer
// keys only authenticate gasless wallet batches; they cannot place orders.
func (e *Engine) ProbeRelayer(ctx context.Context, v market.Venue, settings LiveSettings, clobBaseURL, apiKey, address string) (RelayerProbe, error) {
	v = effectiveVenue(v)
	settings = settings.MergeSecret(e.SystemSettings().Live).Normalize()
	if err := settings.Validate(); err != nil {
		return RelayerProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	exec, err := e.executor(v)
	if err != nil {
		return RelayerProbe{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	probe, ok := exec.(relayerProbeRuntime)
	if !ok || probe == nil {
		return RelayerProbe{}, fmt.Errorf("%w: %s 暂不支持 Relayer 凭据检测", ErrInvalidSettings, v.Label())
	}
	return probe.ProbeRelayer(ctx, settings, clobBaseURL, apiKey, address)
}

// DiscoverWallet resolves the Polymarket account wallet (the funder) of the
// configured signer and detects its wallet type (EOA / Proxy / Safe / Deposit).
// It works before a funder is set, so the settings page can fill both fields
// instead of asking the operator to copy an address and guess a type.
func (e *Engine) DiscoverWallet(ctx context.Context, v market.Venue, settings LiveSettings, endpoints WalletEndpoints) (WalletDiscovery, error) {
	v = effectiveVenue(v)
	if v != market.VenuePolymarket {
		return WalletDiscovery{}, fmt.Errorf("%w: %s 不支持钱包自动检测", ErrInvalidSettings, v.Label())
	}
	settings = settings.MergeSecret(e.SystemSettings().Live).Normalize()
	runtime := e.SystemRuntime()
	if strings.TrimSpace(endpoints.Gamma) == "" {
		endpoints.Gamma = runtime.GammaBaseURL
	}
	exec, err := e.executor(v)
	if err != nil {
		return WalletDiscovery{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	discovery, ok := exec.(walletDiscovery)
	if !ok || discovery == nil {
		return WalletDiscovery{}, fmt.Errorf("当前实盘执行器不支持钱包自动检测")
	}
	signer := ""
	// An empty candidate key is not enough to identify the signer: the executor
	// falls back to the process key, and the browser may have just cleared a
	// stored key. Caching by the currently effective signer here would let a
	// discovery made for the old key verify a funder for the new one, so only an
	// explicit key is cacheable.
	if strings.TrimSpace(settings.PrivateKey) != "" {
		signer = walletSignerIdentity(settings.PrivateKey, "")
	}
	if signer != "" {
		if cached, ok := e.cachedWalletDiscovery(signer, settings); ok {
			return cached, nil
		}
	}
	result, err := discovery.DiscoverWallet(ctx, settings, endpoints)
	if err != nil {
		// Failed discoveries are never cached: a transient RPC or profile failure
		// must not be remembered as proof that a wallet is unusable.
		return result, err
	}
	if signer != "" {
		e.cacheWalletDiscovery(signer, result)
	}
	return result, nil
}

// applyRuntimeSettings applies proxy, per-venue data-source endpoints and the
// Polymarket live credentials in order. If a later step fails, earlier steps
// are rolled back so an unusable value never becomes effective.
func (e *Engine) applyRuntimeSettings(settings SystemSettings) error {
	settings = settings.Normalize()
	previous := e.SystemSettings()
	if e.proxy != nil {
		if err := e.proxy.Apply(settings.Proxy); err != nil {
			return err
		}
	}
	type restore struct {
		provider dataSourceRuntime
		baseURL  string
	}
	var restored []restore
	// Roll back every object that may already have been switched, not only the
	// data-source URLs. A Polymarket executor updated before a Predict.fun
	// failure would otherwise keep signing with the rejected candidate.
	rollback := func() error {
		var errs []error
		for i := len(restored) - 1; i >= 0; i-- {
			if err := restored[i].provider.SetBaseURL(restored[i].baseURL); err != nil {
				errs = append(errs, err)
			}
		}
		if e.proxy != nil {
			if err := e.proxy.Apply(previous.Proxy); err != nil {
				errs = append(errs, err)
			}
		}
		if updater, ok := e.venues.Executor(market.VenuePolymarket).(liveSettingsRuntime); ok && updater != nil {
			pm := previous.Venues[market.VenuePolymarket]
			if err := updater.UpdateSettings(pm.Live.LiveSettings, pm.DataSource.CLOBBaseURL); err != nil {
				errs = append(errs, err)
			}
		}
		if updater, ok := e.venues.Executor(market.VenuePredictFun).(venueCredentialResetter); ok && updater != nil {
			if err := updater.ResetLiveSettings(); err != nil {
				errs = append(errs, err)
			}
		}
		if pf, ok := previous.Venues[market.VenuePredictFun]; ok {
			if updater, ok := e.venues.Executor(market.VenuePredictFun).(venueCredentialRuntime); ok && updater != nil {
				if err := updater.ApplyLiveSettings(pf.Live.APIKey, pf.Live.PrivateKey, pf.Live.SmartWallet, pf.Live.Testnet, pf.DataSource.BaseURL); err != nil {
					errs = append(errs, err)
				}
			}
		}
		return errors.Join(errs...)
	}
	for _, v := range e.Venues() {
		provider, ok := e.data(v).(dataSourceRuntime)
		if !ok || provider == nil {
			continue
		}
		restored = append(restored, restore{provider: provider, baseURL: provider.BaseURL()})
		if err := provider.SetBaseURL(venueBaseURL(settings, v)); err != nil {
			return errors.Join(err, rollback())
		}
	}
	if updater, ok := e.venues.Executor(market.VenuePolymarket).(liveSettingsRuntime); ok && updater != nil {
		pm := settings.Venues[market.VenuePolymarket]
		if err := updater.UpdateSettings(pm.Live.LiveSettings, pm.DataSource.CLOBBaseURL); err != nil {
			return errors.Join(err, rollback())
		}
	}
	// Predict.fun shares one configuration between its data client and its
	// executor, so the executor applies the stored credentials directly. They
	// are validated before the swap, which keeps the previous ones effective
	// when a stored row is unusable. An absent venue block means "inherit the
	// process environment", so the constructor's credentials are left alone.
	if pf, ok := settings.Venues[market.VenuePredictFun]; ok {
		if updater, ok := e.venues.Executor(market.VenuePredictFun).(venueCredentialRuntime); ok && updater != nil {
			err := updater.ApplyLiveSettings(pf.Live.APIKey, pf.Live.PrivateKey, pf.Live.SmartWallet, pf.Live.Testnet, pf.DataSource.BaseURL)
			if err != nil {
				return errors.Join(err, rollback())
			}
		}
	}
	return nil
}

// venueCredentialRuntime lets the engine hand a venue its stored credentials
// without a restart. Predict.fun implements it on the data client and on the
// executor, which share one atomic configuration.
type venueCredentialRuntime interface {
	ApplyLiveSettings(apiKey, privateKey, smartWallet string, testnet *bool, baseURL string) error
}

// venueCredentialResetter restores the startup configuration. Credential
// application treats empty strings as "keep", so it cannot express "go back to
// the process wallet"; a reset can, and a test run must never leave the
// executor holding candidate credentials.
type venueCredentialResetter interface {
	ResetLiveSettings() error
}

// probePredictFunLive checks candidate credentials without saving them. The
// candidate is applied, the collateral balance is read through the chain
// endpoint, and the stored credentials are restored afterwards so a test never
// leaves the executor half-configured.
func (e *Engine) probePredictFunLive(ctx context.Context, v market.Venue, candidate VenueLiveSettings, baseURL string) (LiveProbe, error) {
	// The probe swaps credentials on the executor that live orders also use, so
	// it runs under the same mutex: a scan must not sign an order with the
	// candidate key, and a half-applied probe must not race a settings save.
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	exec, err := e.executor(v)
	if err != nil {
		return LiveProbe{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	runtime, ok := exec.(venueCredentialRuntime)
	if !ok || runtime == nil {
		return LiveProbe{}, fmt.Errorf("%w: %s 暂不支持凭据检测", ErrInvalidSettings, v.Label())
	}
	stored := e.SystemSettings().Venues[v].Live
	value := func(candidate, stored string) string {
		if candidate == "" || candidate == netproxy.RedactedSecret {
			return stored
		}
		return candidate
	}
	apiKey := value(strings.TrimSpace(candidate.APIKey), stored.APIKey)
	privateKey := value(candidate.PrivateKey, stored.PrivateKey)
	smartWallet := value(strings.TrimSpace(candidate.SmartWallet), stored.SmartWallet)
	testnet := candidate.Testnet
	effectiveTestnet := false
	if testnet != nil {
		effectiveTestnet = *testnet
	} else if stored.Testnet != nil {
		effectiveTestnet = *stored.Testnet
	}
	if err := runtime.ApplyLiveSettings(apiKey, privateKey, smartWallet, testnet, strings.TrimSpace(baseURL)); err != nil {
		return LiveProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	defer e.restoreLiveCredentials(v, exec, runtime, stored)
	if message := exec.ReadyError(); message != "" {
		return LiveProbe{}, fmt.Errorf("%w: %s", ErrInvalidSettings, message)
	}
	start := time.Now()
	snapshot, err := exec.Collateral(ctx)
	if err != nil {
		return LiveProbe{}, err
	}
	return LiveProbe{
		Signer:    exec.Signer(),
		Maker:     exec.Maker(),
		APIKey:    maskCredential(apiKey),
		Source:    "chain",
		Mode:      "live",
		Cash:      snapshot.Amount,
		Allowance: snapshot.Allowance,
		LatencyMS: float64(time.Since(start).Milliseconds()),
		Testnet:   effectiveTestnet,
	}, nil
}

// restoreLiveCredentials puts the executor back on its startup configuration and
// then re-applies what the operator saved. A reset alone would drop persisted
// credentials; re-applying alone would keep the candidate's wallet, because an
// empty string means "keep" rather than "inherit".
func (e *Engine) restoreLiveCredentials(v market.Venue, exec venue.Executor, runtime venueCredentialRuntime, stored VenueLiveSettings) {
	var restoreErr error
	if resetter, ok := exec.(venueCredentialResetter); ok {
		if err := resetter.ResetLiveSettings(); err == nil {
			if block, exists := e.SystemSettings().Venues[v]; exists {
				restoreErr = runtime.ApplyLiveSettings(block.Live.APIKey, block.Live.PrivateKey, block.Live.SmartWallet, block.Live.Testnet, block.DataSource.BaseURL)
			}
		} else {
			restoreErr = err
		}
	} else {
		restoreErr = runtime.ApplyLiveSettings(stored.APIKey, stored.PrivateKey, stored.SmartWallet, stored.Testnet, "")
	}
	if restoreErr == nil {
		return
	}
	// A probe that cannot put the saved credentials back leaves the executor on
	// the tested candidate, so the failure has to be visible: swallowing it would
	// let a "credential check" silently switch what the next order signs with.
	e.mu.Lock()
	e.systemError = fmt.Sprintf("%s 凭据检测后恢复已存配置失败：%v", v.Label(), restoreErr)
	e.mu.Unlock()
}

// maskCredential keeps the identifying prefix of an API key, matching how the
// Polymarket probe reports it.
func maskCredential(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "…"
}

// venueBaseURL is the data-source override for one venue. Polymarket keeps the
// legacy gamma_base_url field; Predict.fun uses its own base_url.
func venueBaseURL(settings SystemSettings, v market.Venue) string {
	entry, ok := settings.Venues[v]
	if !ok {
		return ""
	}
	if v == market.VenuePredictFun {
		return entry.DataSource.BaseURL
	}
	return entry.DataSource.GammaBaseURL
}
