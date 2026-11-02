package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/venue"
)

var (
	ErrInvalidOrder    = errors.New("订单不符合策略或风控条件")
	ErrNotFound        = errors.New("市场或结果不存在")
	ErrUpstream        = errors.New("公开行情不可用")
	ErrLiveUnavailable = errors.New("实盘交易不可用")
	ErrAccountNotFound = errors.New("账户不存在")
	ErrAccountConflict = errors.New("账户状态冲突")
	ErrInvalidSettings = errors.New("系统设置无效")
	// ErrVenueMismatch means an account was asked to trade a market that
	// belongs to another venue. Prices and balances are not comparable across
	// venues, so this is a client error rather than something to route around.
	ErrVenueMismatch = errors.New("该机会不属于此账户的场所")
	// ErrVenueReadOnly means the venue is registered for market data but has no
	// executor, so orders must be refused instead of silently falling back to
	// another venue's executor.
	ErrVenueReadOnly = errors.New("该场所当前只读")
	// ErrNoApproval means the venue needs no on-chain allowance at all (its
	// orders are signed through a proxy or a custodian), so an approval request
	// is a client mistake rather than a chain failure.
	ErrNoApproval = errors.New("该场所无需链上授权")
	// ErrVenueNotConfigured means the venue has an executor but it is missing
	// credentials or is not otherwise ready to trade.
	ErrVenueNotConfigured = errors.New("该场所实盘未配置")
)

// venueError carries a contract-exact message while still matching a sentinel
// through errors.Is. The API's error table specifies the user-facing text per
// scenario, so it cannot use the default "sentinel: detail" formatting.
type venueError struct {
	kind    error
	message string
}

func (e *venueError) Error() string { return e.message }
func (e *venueError) Unwrap() error { return e.kind }

type AccountMode string

const (
	AccountModeLive  AccountMode = "live"
	AccountModePaper AccountMode = "paper"
)

func (m AccountMode) Valid() bool {
	return m == AccountModeLive || m == AccountModePaper
}

// Account is an isolated portfolio. Live accounts route orders to their own
// venue's executor; paper accounts only write local simulated positions. An
// account belongs to exactly one venue, which is what keeps two collateral
// currencies from ever being summed.
type Account struct {
	ID   int64       `json:"id"`
	Name string      `json:"name"`
	Mode AccountMode `json:"mode"`
	// Venue is the exchange this account trades on. Empty means Polymarket so
	// accounts created before multi-venue support keep working.
	Venue          market.Venue    `json:"venue"`
	Currency       market.Currency `json:"currency"`
	IsDefault      bool            `json:"is_default"`
	AutoTrade      bool            `json:"auto_trade"`
	BaselineEquity float64         `json:"baseline_equity"`
	CreatedAt      time.Time       `json:"created_at"`

	// Live* are computed at read time and are not persisted with the account.
	LiveConfigured bool   `json:"live_configured"`
	LiveReady      bool   `json:"live_ready"`
	LiveSigner     string `json:"live_signer"`
	LiveMaker      string `json:"live_maker"`
	LiveError      string `json:"live_error"`
}

// EffectiveVenue defaults an unset venue to Polymarket. Every account rows that
// predates multi-venue support was a Polymarket account.
func (a Account) EffectiveVenue() market.Venue {
	if a.Venue == "" {
		return market.VenuePolymarket
	}
	return a.Venue
}

// EffectiveCurrency is the account's collateral asset. It follows the venue so
// a legacy row without a currency column still reports USDC.
func (a Account) EffectiveCurrency() market.Currency {
	if a.Currency != "" {
		return a.Currency
	}
	return currencyForVenue(a.EffectiveVenue())
}

// currencyForVenue is the only place this build decides which collateral a
// venue uses. Polymarket is USDC, Predict.fun is USDT on BNB Chain.
func currencyForVenue(v market.Venue) market.Currency {
	if v == market.VenuePredictFun {
		return market.CurrencyUSDT
	}
	return market.CurrencyUSDC
}

// effectiveVenue normalizes the empty-string legacy value without rejecting an
// unknown venue, which validation must still be able to report.
func effectiveVenue(v market.Venue) market.Venue {
	if v == "" {
		return market.VenuePolymarket
	}
	return v
}

// FeeModelVersion is bumped whenever the default fee accounting changes so a
// database written by an older build can be upgraded exactly once on load.
const FeeModelVersion = 2

// VenueSettings are the per-venue overrides an operator can set. A zero value
// inherits the matching global Settings field, so a payload written before
// venues existed keeps behaving exactly as before.
type VenueSettings struct {
	Enabled bool `json:"enabled"`
	// MinLiquidity overrides the global floor for this venue. Predict.fun books
	// are far thinner than Polymarket's, so one global number either hides every
	// Predict.fun row or lets Polymarket noise through. Zero means "inherit".
	MinLiquidity float64 `json:"min_liquidity"`
	OrderSize    float64 `json:"order_size"`
	MaxPosition  float64 `json:"max_position"`
	MaxExposure  float64 `json:"max_exposure"`
	FeeBPS       float64 `json:"fee_bps"`
}

// EffectiveSettings is one venue's settings after inheriting the global values.
// Scan gates, sizing, exposure caps and the paper fee model all read this type,
// which is what makes the caps per venue by construction.
type EffectiveSettings struct {
	Venue        market.Venue `json:"venue"`
	Enabled      bool         `json:"enabled"`
	MinLiquidity float64      `json:"min_liquidity"`
	OrderSize    float64      `json:"order_size"`
	MaxPosition  float64      `json:"max_position"`
	MaxExposure  float64      `json:"max_exposure"`
	FeeBPS       float64      `json:"fee_bps"`
}

// defaultVenueEnabled is the upgrade-safe default: an existing installation
// keeps trading Polymarket, and a venue added later stays off until an operator
// enables it explicitly. Without this, an upgrade could silently start trading
// a new exchange with real money.
func defaultVenueEnabled(v market.Venue) bool {
	return v != market.VenuePredictFun
}

// For resolves a venue's effective settings. An absent venue entry inherits
// every global value plus the venue's default enabled state; a present entry is
// read literally, so an operator who saves {"enabled": false} really disables
// the venue.
func (s Settings) For(v market.Venue) EffectiveSettings {
	v = effectiveVenue(v)
	effective := EffectiveSettings{
		Venue: v, Enabled: defaultVenueEnabled(v),
		MinLiquidity: s.MinLiquidity,
		OrderSize:    s.OrderSize, MaxPosition: s.MaxPosition, MaxExposure: s.MaxExposure, FeeBPS: s.FeeBPS,
	}
	override, ok := s.Venues[v]
	if !ok {
		return effective
	}
	effective.Enabled = override.Enabled
	if override.MinLiquidity > 0 {
		effective.MinLiquidity = override.MinLiquidity
	}
	if override.OrderSize > 0 {
		effective.OrderSize = override.OrderSize
	}
	if override.MaxPosition > 0 {
		effective.MaxPosition = override.MaxPosition
	}
	if override.MaxExposure > 0 {
		effective.MaxExposure = override.MaxExposure
	}
	if override.FeeBPS > 0 {
		effective.FeeBPS = override.FeeBPS
	}
	return effective
}

type Settings struct {
	MinPrice            float64 `json:"min_price"`
	MaxPrice            float64 `json:"max_price"`
	MaxDays             int     `json:"max_days"`
	MinLiquidity        float64 `json:"min_liquidity"`
	OrderSize           float64 `json:"order_size"`
	MaxPosition         float64 `json:"max_position"`
	MaxExposure         float64 `json:"max_exposure"`
	ScanIntervalSeconds int     `json:"scan_interval_seconds"`
	SlippageBPS         float64 `json:"slippage_bps"`
	FeeBPS              float64 `json:"fee_bps"`
	InitialBankroll     float64 `json:"initial_bankroll"`
	// DefaultCategory only controls which category the radar opens with. It
	// does not change automatic trading; SportsPolicy does.
	DefaultCategory string `json:"default_category"`
	// SportsPolicy is one of exclude_games, exclude_all or allow. The default
	// excludes game markets because overtime and late-game swings are poorly
	// suited to a 0.95–0.99 tail strategy.
	SportsPolicy string `json:"sports_policy"`
	// MaxPriceChange24h rejects markets whose reference price moved more than
	// this amount in the last 24 hours. Zero disables the guard.
	MaxPriceChange24h float64 `json:"max_price_change_24h"`
	// EntryWindowMinutes only lets automatic buys happen during the last N
	// minutes before a market closes. Sports markets use
	// SportsEntryWindowMinutes instead because their result can still swing in
	// overtime. Zero disables the gate for that group.
	EntryWindowMinutes       int `json:"entry_window_minutes"`
	SportsEntryWindowMinutes int `json:"sports_entry_window_minutes"`
	// StopLossEnabled turns the local stop-loss monitor on. Polymarket's CLOB
	// has no native stop order, so the engine watches the mark price on every
	// scan and submits a market sell when the trigger is hit.
	StopLossEnabled bool `json:"stop_loss_enabled"`
	// StopLossRatio is the fraction of the position's average entry price at
	// which the stop triggers. 0.70 means "exit when the mark falls to 70% of
	// the entry price", not an absolute price of 0.70.
	StopLossRatio float64 `json:"stop_loss_ratio"`
	// FeeModelVersion is managed by the server; clients must not set it.
	FeeModelVersion int `json:"fee_model_version"`
	// Venues holds per-venue overrides and the per-venue paper fee. Missing
	// venues fall back to the global values plus their default enabled state.
	Venues map[market.Venue]VenueSettings `json:"venues,omitempty"`
}

// MaxEntryWindowMinutes bounds the pre-close entry window. A window longer
// than a day has no practical use for this strategy and would mostly mask a
// configuration mistake.
const MaxEntryWindowMinutes = 1440

const (
	RadarCategoryAll       = "all"
	RadarCategoryNonSports = "non_sports"

	SportsPolicyExcludeGames = "exclude_games"
	SportsPolicyExcludeAll   = "exclude_all"
	SportsPolicyAllow        = "allow"
)

func validRadarCategory(value string) bool {
	switch value {
	case RadarCategoryAll, RadarCategoryNonSports,
		string(market.CategorySports), string(market.CategoryPolitics), string(market.CategoryCrypto),
		string(market.CategoryFinance), string(market.CategoryTech), string(market.CategoryCulture),
		string(market.CategoryOther):
		return true
	default:
		return false
	}
}

func validSportsPolicy(value string) bool {
	switch value {
	case SportsPolicyExcludeGames, SportsPolicyExcludeAll, SportsPolicyAllow:
		return true
	default:
		return false
	}
}

// DefaultFeeBPS approximates Polymarket's taker fee for this strategy's price
// band. The exchange charges fee = shares × rate × p × (1 - p), so as a share
// of the traded notional that is rate × (1 - p): about 25 bps at p=0.95 and
// 5 bps at p=0.99 for a 0.05 category rate. 15 bps matches p≈0.97 and stays
// slightly conservative across the 0.95–0.99 range. Live accounts never use
// this value: they read the market's real FeeSchedule.
const DefaultFeeBPS = 15

// DefaultStopLossRatio exits a position when its mark falls to 70% of the
// average entry price. A stop-loss only reduces a tail-risk loss; a binary
// market can still gap through the trigger and fill far lower.
const DefaultStopLossRatio = 0.70

func DefaultSettings() Settings {
	return Settings{
		MinPrice: 0.95, MaxPrice: 0.99, MaxDays: 30, MinLiquidity: 10000,
		OrderSize: 100, MaxPosition: 250, MaxExposure: 1000,
		ScanIntervalSeconds: 60, SlippageBPS: 20, FeeBPS: DefaultFeeBPS,
		InitialBankroll: 10000, DefaultCategory: RadarCategoryNonSports,
		SportsPolicy: SportsPolicyExcludeGames, MaxPriceChange24h: 0.10,
		EntryWindowMinutes: 20, SportsEntryWindowMinutes: 5,
		StopLossEnabled: false, StopLossRatio: DefaultStopLossRatio,
		FeeModelVersion: FeeModelVersion,
		// Polymarket stays enabled for existing installations; Predict.fun is
		// opt-in because it was never traded by this build before.
		Venues: map[market.Venue]VenueSettings{
			market.VenuePolymarket: {Enabled: true},
			market.VenuePredictFun: {Enabled: false},
		},
	}
}

func (s Settings) Validate() error {
	finitePositive := func(value float64) bool { return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) }
	if !finitePositive(s.MinPrice) || !finitePositive(s.MaxPrice) || s.MinPrice > s.MaxPrice || s.MaxPrice >= 1 {
		return fmt.Errorf("价格区间须满足 0 < 最低价 ≤ 最高价 < 1")
	}
	if s.MaxDays < 1 || s.MaxDays > 3650 {
		return fmt.Errorf("到期范围须为 1–3650 天")
	}
	if s.MinLiquidity < 0 || math.IsNaN(s.MinLiquidity) || math.IsInf(s.MinLiquidity, 0) {
		return fmt.Errorf("最低流动性不能小于 0")
	}
	if !finitePositive(s.OrderSize) || !finitePositive(s.MaxPosition) || !finitePositive(s.MaxExposure) || !finitePositive(s.InitialBankroll) {
		return fmt.Errorf("订单、持仓、敞口和初始资金必须为有限正数")
	}
	if s.MaxPosition < s.OrderSize {
		return fmt.Errorf("单市场敞口上限不能小于单笔金额")
	}
	if s.MaxExposure < s.OrderSize {
		return fmt.Errorf("总敞口上限不能小于单笔金额")
	}
	if s.ScanIntervalSeconds < 10 || s.ScanIntervalSeconds > 86400 {
		return fmt.Errorf("扫描间隔须为 10–86400 秒")
	}
	if s.SlippageBPS < 0 || s.SlippageBPS > 1000 || s.FeeBPS < 0 || s.FeeBPS > 1000 ||
		math.IsNaN(s.SlippageBPS) || math.IsNaN(s.FeeBPS) || math.IsInf(s.SlippageBPS, 0) || math.IsInf(s.FeeBPS, 0) {
		return fmt.Errorf("滑点和费率须为 0–1000 BPS")
	}
	if !validRadarCategory(s.DefaultCategory) {
		return fmt.Errorf("默认分类无效")
	}
	if !validSportsPolicy(s.SportsPolicy) {
		return fmt.Errorf("体育市场策略无效")
	}
	if s.MaxPriceChange24h < 0 || s.MaxPriceChange24h > 1 ||
		math.IsNaN(s.MaxPriceChange24h) || math.IsInf(s.MaxPriceChange24h, 0) {
		return fmt.Errorf("24 小时价格波动上限须为 0–1")
	}
	if s.EntryWindowMinutes < 0 || s.EntryWindowMinutes > MaxEntryWindowMinutes {
		return fmt.Errorf("非体育尾盘下单窗口须为 0–%d 分钟", MaxEntryWindowMinutes)
	}
	if s.SportsEntryWindowMinutes < 0 || s.SportsEntryWindowMinutes > MaxEntryWindowMinutes {
		return fmt.Errorf("体育尾盘下单窗口须为 0–%d 分钟", MaxEntryWindowMinutes)
	}
	if s.StopLossRatio <= 0 || s.StopLossRatio >= 1 ||
		math.IsNaN(s.StopLossRatio) || math.IsInf(s.StopLossRatio, 0) {
		return fmt.Errorf("止损比例须为 0–1 之间的小数（例如 0.70 表示入场价的 70%%）")
	}
	for v := range s.Venues {
		if !v.Valid() {
			return fmt.Errorf("%w: 未知场所 %q", ErrInvalidSettings, v)
		}
		effective := s.For(v)
		label := v.Label()
		if !finitePositive(effective.OrderSize) {
			return fmt.Errorf("%w: %s 的单笔金额必须为有限正数", ErrInvalidSettings, label)
		}
		if !finitePositive(effective.MaxPosition) || effective.MaxPosition < effective.OrderSize {
			return fmt.Errorf("%w: %s 的单市场敞口上限不能小于单笔金额", ErrInvalidSettings, label)
		}
		if !finitePositive(effective.MaxExposure) || effective.MaxExposure < effective.MaxPosition {
			return fmt.Errorf("%w: %s 的总敞口上限不能小于单市场敞口上限", ErrInvalidSettings, label)
		}
		if effective.FeeBPS < 0 || effective.FeeBPS > 1000 ||
			math.IsNaN(effective.FeeBPS) || math.IsInf(effective.FeeBPS, 0) {
			return fmt.Errorf("%w: %s 的费率须为 0–1000 BPS", ErrInvalidSettings, label)
		}
		if effective.MinLiquidity < 0 || math.IsNaN(effective.MinLiquidity) || math.IsInf(effective.MinLiquidity, 0) {
			return fmt.Errorf("%w: %s 的最低流动性不能小于 0", ErrInvalidSettings, label)
		}
	}
	return nil
}

// SystemSettings holds runtime settings that are edited in the web UI instead
// of the process environment. They are persisted in SQLite so they survive a
// restart without leaking into shell history or .env files. Empty strings mean
// "inherit the process environment / built-in default", which keeps existing
// deployments working while allowing the UI to override them at runtime.
type SystemSettings struct {
	Proxy      netproxy.Config    `json:"proxy"`
	DataSource DataSourceSettings `json:"data_source"`
	Live       LiveSettings       `json:"live"`
	// Venues is the per-venue view of the same settings. The flat fields above
	// remain the Polymarket view for older frontends and databases: a payload
	// without venues.polymarket inherits them, and every save writes both.
	Venues map[market.Venue]VenueSystemSettings `json:"venues,omitempty"`
}

// VenueSystemSettings is one venue's block inside SystemSettings. The
// Polymarket block mirrors the flat legacy fields; Predict.fun's live block
// carries its own BNB-chain credentials.
type VenueSystemSettings struct {
	DataSource VenueDataSourceSettings `json:"data_source"`
	Live       VenueLiveSettings       `json:"live"`
}

// VenueDataSourceSettings holds the public endpoints one venue can override.
// Polymarket uses gamma_base_url/clob_base_url; Predict.fun uses base_url.
type VenueDataSourceSettings struct {
	GammaBaseURL string `json:"gamma_base_url,omitempty"`
	CLOBBaseURL  string `json:"clob_base_url,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
}

// VenueLiveSettings is the credential set a venue needs to trade. The embedded
// LiveSettings keeps the Polymarket shape and its redaction rules; the extra
// fields are Predict.fun's independent BNB-chain wallet.
type VenueLiveSettings struct {
	LiveSettings
	SmartWallet string `json:"smart_wallet,omitempty"`
	// Testnet is tri-state on purpose: absent means "inherit the process
	// environment", an explicit value switches networks. A plain bool would make
	// saving an unrelated field silently move a testnet deployment to mainnet
	// (real USDT, mainnet contracts).
	Testnet *bool `json:"testnet,omitempty"`
}

func (d VenueDataSourceSettings) Normalize() VenueDataSourceSettings {
	d.GammaBaseURL = strings.TrimRight(strings.TrimSpace(d.GammaBaseURL), "/")
	d.CLOBBaseURL = strings.TrimRight(strings.TrimSpace(d.CLOBBaseURL), "/")
	d.BaseURL = strings.TrimRight(strings.TrimSpace(d.BaseURL), "/")
	return d
}

func (d VenueDataSourceSettings) Validate() error {
	d = d.Normalize()
	if err := validateBaseURL("Gamma 行情地址", d.GammaBaseURL); err != nil {
		return err
	}
	if err := validateBaseURL("CLOB 交易地址", d.CLOBBaseURL); err != nil {
		return err
	}
	return validateBaseURL("行情地址", d.BaseURL)
}

func (s VenueLiveSettings) Normalize() VenueLiveSettings {
	s.LiveSettings = s.LiveSettings.Normalize()
	s.SmartWallet = normalizeHexPrefix(strings.TrimSpace(s.SmartWallet))
	// funder is the Polymarket name for the same thing (the address that holds
	// the money and signs differently). The venue block embeds LiveSettings, so
	// a client that only knows the Polymarket spelling would otherwise fill a
	// field this venue never reads and silently lose its maker address.
	if s.SmartWallet == "" && s.Funder != "" {
		s.SmartWallet = s.Funder
	}
	return s
}

func (s VenueLiveSettings) Validate(v market.Venue) error {
	if v == market.VenuePredictFun {
		// Predict.fun authenticates with x-api-key plus an EOA signature; it has
		// no L2 secret/passphrase triple and no Polymarket funder model, so the
		// Polymarket validator would reject a valid Predict.fun credential set.
		if s.PrivateKey != "" && !validHexValue(s.PrivateKey, 32) {
			return fmt.Errorf("Predict.fun 私钥必须是 0x 开头的 64 位十六进制字符串")
		}
		if s.SmartWallet != "" && !validHexValue(s.SmartWallet, 20) {
			return fmt.Errorf("Predict.fun 的智能钱包必须是 0x 开头的 40 位十六进制地址")
		}
		return nil
	}
	if err := s.LiveSettings.Validate(); err != nil {
		return err
	}
	if s.SmartWallet != "" && !validHexValue(s.SmartWallet, 20) {
		return fmt.Errorf("%s 的智能钱包必须是 0x 开头的 40 位十六进制地址", v.Label())
	}
	return nil
}

func (s VenueLiveSettings) Redacted() VenueLiveSettings {
	s.LiveSettings = s.LiveSettings.Redacted()
	return s
}

func (s VenueLiveSettings) MergeSecret(previous VenueLiveSettings) VenueLiveSettings {
	s.LiveSettings = s.LiveSettings.MergeSecret(previous.LiveSettings)
	if s.SmartWallet == "" || s.SmartWallet == netproxy.RedactedSecret {
		s.SmartWallet = previous.SmartWallet
	}
	if s.Testnet == nil {
		s.Testnet = previous.Testnet
	}
	return s.Normalize()
}

func (s VenueSystemSettings) Normalize() VenueSystemSettings {
	s.DataSource = s.DataSource.Normalize()
	s.Live = s.Live.Normalize()
	return s
}

func (s VenueSystemSettings) Validate(v market.Venue) error {
	if !v.Valid() {
		return fmt.Errorf("未知场所 %q", v)
	}
	if err := s.DataSource.Validate(); err != nil {
		return fmt.Errorf("%s: %w", v.Label(), err)
	}
	return s.Live.Validate(v)
}

func (s VenueSystemSettings) Redacted() VenueSystemSettings {
	s.Live = s.Live.Redacted()
	return s
}

// Merge treats an empty field as "keep the stored value", matching the PUT
// semantics of the flat fields so a partial venue update cannot wipe another
// venue's endpoints or credentials.
func (s VenueSystemSettings) Merge(previous VenueSystemSettings) VenueSystemSettings {
	if s.DataSource.GammaBaseURL == "" {
		s.DataSource.GammaBaseURL = previous.DataSource.GammaBaseURL
	}
	if s.DataSource.CLOBBaseURL == "" {
		s.DataSource.CLOBBaseURL = previous.DataSource.CLOBBaseURL
	}
	if s.DataSource.BaseURL == "" {
		s.DataSource.BaseURL = previous.DataSource.BaseURL
	}
	// A redacted secret echoed back by a client means "keep the stored value",
	// exactly like an empty field: storing the placeholder instead would fail
	// validation and reject an otherwise harmless round-trip.
	keep := func(value string) bool {
		return value == "" || value == netproxy.RedactedSecret
	}
	if keep(s.Live.PrivateKey) {
		s.Live.PrivateKey = previous.Live.PrivateKey
	}
	if s.Live.Funder == "" {
		s.Live.Funder = previous.Live.Funder
	}
	if s.Live.SignatureType == nil {
		s.Live.SignatureType = previous.Live.SignatureType
	}
	if s.Live.APIKey == "" {
		s.Live.APIKey = previous.Live.APIKey
	}
	if keep(s.Live.APISecret) {
		s.Live.APISecret = previous.Live.APISecret
	}
	if keep(s.Live.APIPassphrase) {
		s.Live.APIPassphrase = previous.Live.APIPassphrase
	}
	if keep(s.Live.SmartWallet) {
		s.Live.SmartWallet = previous.Live.SmartWallet
	}
	// Testnet is three-state: nil means "inherit". Without this a PUT that
	// touches any unrelated field of the venue block would quietly drop a stored
	// testnet:true and the next process start would fall back to mainnet.
	if s.Live.Testnet == nil {
		s.Live.Testnet = previous.Live.Testnet
	}
	return s.Normalize()
}

// DataSourceSettings overrides the public data and CLOB endpoints. An empty
// value falls back to GAMMA_BASE_URL / POLYMARKET_CLOB_URL or the built-in
// public defaults.
type DataSourceSettings struct {
	GammaBaseURL string `json:"gamma_base_url"`
	CLOBBaseURL  string `json:"clob_base_url"`
}

func (d DataSourceSettings) Normalize() DataSourceSettings {
	d.GammaBaseURL = strings.TrimRight(strings.TrimSpace(d.GammaBaseURL), "/")
	d.CLOBBaseURL = strings.TrimRight(strings.TrimSpace(d.CLOBBaseURL), "/")
	return d
}

func (d DataSourceSettings) Validate() error {
	d = d.Normalize()
	if err := validateBaseURL("Gamma 行情地址", d.GammaBaseURL); err != nil {
		return err
	}
	return validateBaseURL("CLOB 交易地址", d.CLOBBaseURL)
}

func validateBaseURL(label, raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s无效: %w", label, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%s仅支持 http 或 https", label)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s缺少主机与端口", label)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s不能包含用户名或密码", label)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s不能包含查询参数或片段", label)
	}
	return nil
}

// LiveSettings are web-managed overrides for the live CLOB executor. Secrets
// are encrypted at rest by the store layer and are never returned to the
// browser in clear text.
type LiveSettings struct {
	PrivateKey    string `json:"private_key"`
	Funder        string `json:"funder"`
	SignatureType *int   `json:"signature_type"`
	APIKey        string `json:"api_key"`
	APISecret     string `json:"api_secret"`
	APIPassphrase string `json:"api_passphrase"`
}

func (s LiveSettings) Normalize() LiveSettings {
	s.PrivateKey = normalizeHexPrefix(strings.TrimSpace(s.PrivateKey))
	s.Funder = normalizeHexPrefix(strings.TrimSpace(s.Funder))
	s.APIKey = strings.TrimSpace(s.APIKey)
	s.APISecret = strings.TrimSpace(s.APISecret)
	s.APIPassphrase = strings.TrimSpace(s.APIPassphrase)
	if s.SignatureType != nil {
		value := *s.SignatureType
		s.SignatureType = &value
	}
	return s
}

func normalizeHexPrefix(value string) string {
	if len(value) >= 2 && strings.EqualFold(value[:2], "0x") {
		return "0x" + value[2:]
	}
	return value
}

func (s LiveSettings) Validate() error {
	s = s.Normalize()
	if s.PrivateKey != "" && !validHexValue(s.PrivateKey, 32) {
		return fmt.Errorf("实盘私钥必须是 0x 开头的 64 位十六进制字符串")
	}
	if s.Funder != "" && !validHexValue(s.Funder, 20) {
		return fmt.Errorf("Funder 必须是 0x 开头的 40 位十六进制地址")
	}
	if s.SignatureType != nil {
		if *s.SignatureType < 0 || *s.SignatureType > 3 {
			return fmt.Errorf("签名类型仅支持 0（EOA）、1（Proxy）、2（Safe）或 3（Deposit/Poly1271）")
		}
		if *s.SignatureType == 0 && s.Funder != "" {
			return fmt.Errorf("EOA 签名类型不需要 Funder，请留空或改用 1/2/3")
		}
	}
	credentials := 0
	for _, value := range []string{s.APIKey, s.APISecret, s.APIPassphrase} {
		if value != "" {
			credentials++
		}
	}
	if credentials != 0 && credentials != 3 {
		return fmt.Errorf("L2 API Key、Secret、Passphrase 必须同时填写或同时留空")
	}
	return nil
}

// Redacted returns a copy safe to send to the browser. The API key is an
// identifier, but the private key and L2 secrets are masked.
func (s LiveSettings) Redacted() LiveSettings {
	s = s.Normalize()
	if s.PrivateKey != "" {
		s.PrivateKey = netproxy.RedactedSecret
	}
	if s.APISecret != "" {
		s.APISecret = netproxy.RedactedSecret
	}
	if s.APIPassphrase != "" {
		s.APIPassphrase = netproxy.RedactedSecret
	}
	return s
}

// MergeSecret restores stored secrets when the browser echoes back the masked
// values it received. An explicit empty value still clears the override.
func (s LiveSettings) MergeSecret(previous LiveSettings) LiveSettings {
	s = s.Normalize()
	previous = previous.Normalize()
	if s.PrivateKey == netproxy.RedactedSecret {
		s.PrivateKey = previous.PrivateKey
	}
	if s.APISecret == netproxy.RedactedSecret {
		s.APISecret = previous.APISecret
	}
	if s.APIPassphrase == netproxy.RedactedSecret {
		s.APIPassphrase = previous.APIPassphrase
	}
	return s
}

func validHexValue(value string, bytes int) bool {
	if len(value) != 2+bytes*2 || !strings.HasPrefix(strings.ToLower(value), "0x") {
		return false
	}
	decoded, err := hex.DecodeString(value[2:])
	return err == nil && len(decoded) == bytes
}

func DefaultSystemSettings() SystemSettings {
	return SystemSettings{
		Proxy: netproxy.DefaultConfig(),
		Venues: map[market.Venue]VenueSystemSettings{
			market.VenuePolymarket: {},
			market.VenuePredictFun: {},
		},
	}
}

func (s SystemSettings) Validate() error {
	if err := s.Proxy.Validate(); err != nil {
		return err
	}
	if err := s.DataSource.Validate(); err != nil {
		return err
	}
	if err := s.Live.Validate(); err != nil {
		return err
	}
	for v, entry := range s.Venues {
		if err := entry.Validate(v); err != nil {
			return err
		}
	}
	// Predict.fun signs on BNB Chain with its own hot wallet. Reusing the
	// Polymarket key would put both venues' funds behind one credential.
	if pf, ok := s.Venues[market.VenuePredictFun]; ok {
		if pm, ok := s.Venues[market.VenuePolymarket]; ok && pf.Live.PrivateKey != "" &&
			pf.Live.PrivateKey == pm.Live.PrivateKey {
			return fmt.Errorf("%s 的实盘私钥不能与 %s 共用", market.VenuePredictFun.Label(), market.VenuePolymarket.Label())
		}
	}
	return nil
}

func (s SystemSettings) Normalize() SystemSettings {
	s.Proxy = s.Proxy.Normalize()
	s.DataSource = s.DataSource.Normalize()
	s.Live = s.Live.Normalize()
	venues := make(map[market.Venue]VenueSystemSettings, len(s.Venues)+1)
	for v, entry := range s.Venues {
		venues[v] = entry.Normalize()
	}
	// Dual read/write for Polymarket, field by field: an absent value inherits
	// the flat legacy field, a present one wins and is mirrored back. This is
	// what keeps a payload without venues.polymarket identical to before.
	pm := venues[market.VenuePolymarket]
	if pm.DataSource.GammaBaseURL == "" {
		pm.DataSource.GammaBaseURL = s.DataSource.GammaBaseURL
	}
	if pm.DataSource.CLOBBaseURL == "" {
		pm.DataSource.CLOBBaseURL = s.DataSource.CLOBBaseURL
	}
	s.DataSource = DataSourceSettings{GammaBaseURL: pm.DataSource.GammaBaseURL, CLOBBaseURL: pm.DataSource.CLOBBaseURL}
	if pm.Live.LiveSettings.isZero() {
		pm.Live.LiveSettings = s.Live
	} else {
		s.Live = pm.Live.LiveSettings
	}
	venues[market.VenuePolymarket] = pm.Normalize()
	s.Venues = venues
	return s
}

// SyncPolymarketVenue mirrors the flat legacy fields into venues.polymarket.
// It is the dual-write half of the system-settings contract: the flat fields
// stay authoritative for the Polymarket runtime, and a reader that only knows
// the venues shape sees the same values.
func (s SystemSettings) SyncPolymarketVenue() SystemSettings {
	s = s.Normalize()
	pm := s.Venues[market.VenuePolymarket]
	pm.DataSource.GammaBaseURL = s.DataSource.GammaBaseURL
	pm.DataSource.CLOBBaseURL = s.DataSource.CLOBBaseURL
	pm.Live.LiveSettings = s.Live
	s.Venues[market.VenuePolymarket] = pm.Normalize()
	return s
}

// isZero reports whether no credential field is set at all. It decides the
// dual-read direction for the Polymarket live block.
func (s LiveSettings) isZero() bool {
	return s.PrivateKey == "" && s.Funder == "" && s.SignatureType == nil &&
		s.APIKey == "" && s.APISecret == "" && s.APIPassphrase == ""
}

func (s SystemSettings) Redacted() SystemSettings {
	s = s.Normalize()
	s.Proxy = s.Proxy.Redacted()
	s.Live = s.Live.Redacted()
	venues := make(map[market.Venue]VenueSystemSettings, len(s.Venues))
	for v, entry := range s.Venues {
		venues[v] = entry.Redacted()
	}
	s.Venues = venues
	return s
}

func (s SystemSettings) MergeSecret(previous SystemSettings) SystemSettings {
	s = s.Normalize()
	previous = previous.Normalize()
	s.Proxy = s.Proxy.MergeSecret(previous.Proxy)
	s.Live = s.Live.MergeSecret(previous.Live)
	for v, entry := range s.Venues {
		entry.Live = entry.Live.MergeSecret(previous.Venues[v].Live)
		s.Venues[v] = entry.Normalize()
	}
	return s
}

// LiveEnvironment reports which values were supplied by the process
// environment at startup. The web UI uses it to show inheritance hints.
type LiveEnvironment struct {
	CLOBBaseURL      string `json:"clob_base_url"`
	PolygonRPCURL    string `json:"polygon_rpc_url"`
	PrivateKeySet    bool   `json:"private_key_set"`
	Funder           string `json:"funder"`
	SignatureType    int    `json:"signature_type"`
	APIKeySet        bool   `json:"api_key_set"`
	APISecretSet     bool   `json:"api_secret_set"`
	APIPassphraseSet bool   `json:"api_passphrase_set"`
}

// LiveProbe is the result of checking candidate live credentials against the
// CLOB. It never carries the private key or the L2 secret; the API key is
// masked because it is an identifier, not a password.
type LiveProbe struct {
	Signer        string  `json:"signer"`
	Maker         string  `json:"maker"`
	APIKey        string  `json:"api_key"`
	Source        string  `json:"source"`
	Mode          string  `json:"mode"`
	SignatureType int     `json:"signature_type"`
	Cash          float64 `json:"cash"`
	Allowance     float64 `json:"allowance"`
	LatencyMS     float64 `json:"latency_ms"`
}

// RelayerProbe is the result of checking a Relayer API key. A relayer key only
// authenticates gasless wallet batches; it never places a CLOB order, so this
// check proves the credential works without touching trading.
type RelayerProbe struct {
	Signer    string  `json:"signer"`
	Address   string  `json:"address"`
	APIKey    string  `json:"api_key"`
	KeyCount  int     `json:"key_count"`
	LatencyMS float64 `json:"latency_ms"`
}

// SystemRuntime is the effective, read-only view of the runtime settings. It
// is not persisted; it is assembled from the registered market data sources
// and executors. The flat Gamma/CLOB fields stay the Polymarket view; Venues
// carries one status per registered venue.
type SystemRuntime struct {
	GammaBaseURL    string          `json:"gamma_base_url"`
	CLOBBaseURL     string          `json:"clob_base_url"`
	GammaDefault    string          `json:"gamma_default"`
	CLOBDefault     string          `json:"clob_default"`
	LiveEnvironment LiveEnvironment `json:"live_environment"`
	LiveConfigured  bool            `json:"live_configured"`
	LiveReady       bool            `json:"live_ready"`
	LiveError       string          `json:"live_error"`
	Venues          []VenueRuntime  `json:"venues"`
	Error           string          `json:"error"`
}

// VenueRuntime is one venue's executor state. Configured means credentials
// were supplied at all; Ready means they passed the venue's own checks.
type VenueRuntime struct {
	Venue      market.Venue `json:"venue"`
	Configured bool         `json:"configured"`
	Ready      bool         `json:"ready"`
	Signer     string       `json:"signer"`
	Maker      string       `json:"maker"`
	Error      string       `json:"error"`
	Testnet    bool         `json:"testnet"`
}

// dataSourceRuntime is implemented by market providers that can switch their
// base URL without a restart.
type dataSourceRuntime interface {
	SetBaseURL(string) error
	BaseURL() string
	DefaultBaseURL() string
}

// liveSettingsRuntime is implemented by the CLOB executor so the engine can
// apply web-managed credentials at runtime.
type liveSettingsRuntime interface {
	UpdateSettings(LiveSettings, string) error
	EffectiveCLOBURL() string
	Environment() LiveEnvironment
}

// liveProbeRuntime is implemented by executors that can validate candidate
// credentials against the CLOB without placing an order.
type liveProbeRuntime interface {
	Probe(context.Context, LiveSettings, string) (LiveProbe, error)
}

// relayerProbeRuntime is implemented by executors that can validate a Relayer
// API key. It is optional because the relayer is not part of order placement.
type relayerProbeRuntime interface {
	ProbeRelayer(context.Context, LiveSettings, string, string, string) (RelayerProbe, error)
}

// WalletEndpoints are the effective endpoints wallet discovery may use. The
// CLOB is kept for the (retired) proxy-wallet endpoint, Gamma answers the
// public profile lookup and RPC is only used for read-only on-chain checks.
type WalletEndpoints struct {
	CLOB  string `json:"clob_base_url"`
	Gamma string `json:"gamma_base_url"`
}

// WalletCandidate is one wallet address that belongs to a signer, together with
// the type that was inferred for it.
type WalletCandidate struct {
	Address    string `json:"address"`
	WalletType int    `json:"wallet_type"`
	WalletName string `json:"wallet_type_name"`
	Deployed   bool   `json:"deployed"`
	Source     string `json:"source"`
}

// WalletDiscovery is the result of resolving a signer to its Polymarket account
// wallet. Funder is empty when the signer is a plain EOA (wallet type 0), which
// is exactly the value the settings page should store.
type WalletDiscovery struct {
	Signer     string            `json:"signer"`
	Funder     string            `json:"funder"`
	WalletType int               `json:"wallet_type"`
	WalletName string            `json:"wallet_type_name"`
	Deployed   bool              `json:"deployed"`
	Source     string            `json:"source"`
	Wallets    []string          `json:"wallets"`
	Candidates []WalletCandidate `json:"candidates"`
	Notes      []string          `json:"notes"`
}

// walletDiscovery is implemented by executors that can resolve the account
// wallet (and its type) of a signer without placing an order.
type walletDiscovery interface {
	DiscoverWallet(context.Context, LiveSettings, WalletEndpoints) (WalletDiscovery, error)
}

type Opportunity struct {
	MarketID       string    `json:"market_id"`
	ConditionID    string    `json:"condition_id"`
	Question       string    `json:"question"`
	QuestionZH     string    `json:"question_zh"`
	GroupItemTitle string    `json:"group_item_title"`
	EventTitleZH   string    `json:"event_title_zh"`
	Slug           string    `json:"slug"`
	Outcome        string    `json:"outcome"`
	OutcomeZH      string    `json:"outcome_zh"`
	TokenID        string    `json:"token_id"`
	Price          float64   `json:"price"`
	Liquidity      float64   `json:"liquidity"`
	Volume         float64   `json:"volume"`
	EndDate        time.Time `json:"end_date"`
	DaysToEnd      float64   `json:"days_to_end"`
	GrossYield     float64   `json:"gross_yield"`
	URL            string    `json:"url"`
	// Category and the risk flags are derived from Polymarket event tags and
	// local heuristics during the scan.
	Category       string   `json:"category"`
	SportsGame     bool     `json:"sports_game"`
	OvertimeRisk   bool     `json:"overtime_risk"`
	PriceChange24h *float64 `json:"price_change_24h,omitempty"`
	// Version is the market's Polymarket protocol version ("v1" or "v2"). Live
	// orders are refused for "v2" because those markets settle through
	// ExchangeV3 with a different signing domain than this build supports.
	Version string `json:"version,omitempty"`
	// Venue and Currency identify which exchange this row belongs to. They are
	// part of the contract because token ids are only unique per venue.
	Venue    market.Venue    `json:"venue"`
	Currency market.Currency `json:"currency"`
	// FeeRateBPS is the taker rate the venue reported for this market. Zero
	// means the venue did not report one and the settings default applies.
	FeeRateBPS float64 `json:"fee_rate_bps,omitempty"`
}

// EffectiveVenue defaults an unset venue to Polymarket.
func (o Opportunity) EffectiveVenue() market.Venue { return effectiveVenue(o.Venue) }

// EffectiveCurrency follows the venue when the provider did not set one.
func (o Opportunity) EffectiveCurrency() market.Currency {
	if o.Currency != "" {
		return o.Currency
	}
	return currencyForVenue(o.EffectiveVenue())
}

// IsV2 reports whether the opportunity trades on Polymarket Protocol V2.
func (o Opportunity) IsV2() bool {
	return strings.EqualFold(strings.TrimSpace(o.Version), "v2")
}

// OpportunityQuote is a read-only, executable estimate for one radar row. It
// walks the real CLOB ask book and applies the market's real taker fee
// schedule, so NetYield is what a live FAK buy of Amount would return if the
// outcome resolves in favour. It never signs or submits an order.
type OpportunityQuote struct {
	TokenID     string          `json:"token_id"`
	ConditionID string          `json:"condition_id"`
	Venue       market.Venue    `json:"venue"`
	Currency    market.Currency `json:"currency"`
	Amount      float64         `json:"amount"`
	BestAsk     float64         `json:"best_ask"`
	// LimitPrice is the worst touched level, tick-ceiled. The live order is
	// rejected when this exceeds the strategy's max_price, even if the average
	// fill price is lower.
	LimitPrice float64 `json:"limit_price"`
	FillPrice  float64 `json:"fill_price"`
	Shares     float64 `json:"shares"`
	Notional   float64 `json:"notional"`
	FeeRate    float64 `json:"fee_rate"`
	Fee        float64 `json:"fee"`
	TotalCost  float64 `json:"total_cost"`
	Payout     float64 `json:"payout"`
	NetProfit  float64 `json:"net_profit"`
	NetYield   float64 `json:"net_yield"`
	GrossYield float64 `json:"gross_yield"`
	// AvailableNotional is the pUSD depth on the ask side at quote time.
	AvailableNotional float64 `json:"available_notional"`
	FullyFilled       bool    `json:"fully_filled"`
	// ExceedsMaxPrice marks a fill price the strategy's max_price would reject.
	ExceedsMaxPrice bool      `json:"exceeds_max_price"`
	MaxPrice        float64   `json:"max_price"`
	QuotedAt        time.Time `json:"quoted_at"`
}

type Position struct {
	ID          int64           `json:"id"`
	AccountID   int64           `json:"account_id"`
	Venue       market.Venue    `json:"venue"`
	Currency    market.Currency `json:"currency"`
	MarketID    string          `json:"market_id"`
	ConditionID string          `json:"condition_id"`
	Question    string          `json:"question"`
	Outcome     string          `json:"outcome"`
	TokenID     string          `json:"token_id"`
	Side        string          `json:"side"`
	Kind        string          `json:"kind"`
	// ExitKind is empty for normal holdings and "stop" for a position closed by
	// the stop-loss monitor. The status itself stays won/lost/split so the
	// existing SQLite CHECK constraints (and any older database) remain valid.
	ExitKind      string     `json:"exit_kind"`
	EntryPrice    float64    `json:"entry_price"`
	CurrentPrice  float64    `json:"current_price"`
	Shares        float64    `json:"shares"`
	Cost          float64    `json:"cost"`
	Fee           float64    `json:"fee"`
	Status        string     `json:"status"`
	OpenedAt      time.Time  `json:"opened_at"`
	ClosedAt      *time.Time `json:"closed_at"`
	EndDate       time.Time  `json:"end_date"`
	RealizedPNL   float64    `json:"realized_pnl"`
	UnrealizedPNL float64    `json:"unrealized_pnl"`
}

// EffectiveVenue defaults an unset venue to Polymarket; legacy rows were all
// Polymarket positions.
func (p Position) EffectiveVenue() market.Venue { return effectiveVenue(p.Venue) }

func (p Position) EffectiveCurrency() market.Currency {
	if p.Currency != "" {
		return p.Currency
	}
	return currencyForVenue(p.EffectiveVenue())
}

type Order struct {
	ID         int64           `json:"id"`
	AccountID  int64           `json:"account_id"`
	Venue      market.Venue    `json:"venue"`
	Currency   market.Currency `json:"currency"`
	PositionID *int64          `json:"position_id"`
	MarketID   string          `json:"market_id"`
	Question   string          `json:"question"`
	Outcome    string          `json:"outcome"`
	TokenID    string          `json:"token_id"`
	Kind       string          `json:"kind"`
	Type       string          `json:"type"`
	// Action is empty for buys/settlements and "stop" for a stop-loss sell.
	// Type stays settle so the orders CHECK constraint remains unchanged.
	Action string  `json:"action"`
	Amount float64 `json:"amount"`
	Price  float64 `json:"price"`
	Shares float64 `json:"shares"`
	// Fee is the taker fee charged for a buy (added to Amount) or a stop-loss
	// sell (already deducted from Amount). Settlement orders carry no fee:
	// Polymarket redeems at par.
	Fee        float64   `json:"fee"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	ExternalID string    `json:"external_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// EffectiveVenue defaults an unset venue to Polymarket.
func (o Order) EffectiveVenue() market.Venue { return effectiveVenue(o.Venue) }

func (o Order) EffectiveCurrency() market.Currency {
	if o.Currency != "" {
		return o.Currency
	}
	return currencyForVenue(o.EffectiveVenue())
}

type EquityPoint struct {
	Venue    market.Venue    `json:"venue"`
	Currency market.Currency `json:"currency"`
	Time     time.Time       `json:"time"`
	Value    float64         `json:"value"`
}

type Metrics struct {
	Mode            string          `json:"mode"`
	Venue           market.Venue    `json:"venue"`
	Currency        market.Currency `json:"currency"`
	InitialBankroll float64         `json:"initial_bankroll"`
	Cash            float64         `json:"cash"`
	WalletCash      float64         `json:"wallet_cash"`
	WalletCashStale bool            `json:"wallet_cash_stale"`
	Equity          float64         `json:"equity"`
	RealizedPNL     float64         `json:"realized_pnl"`
	UnrealizedPNL   float64         `json:"unrealized_pnl"`
	TotalPNL        float64         `json:"total_pnl"`
	// FeesPaid is the sum of the buy-side taker fees already booked into the
	// cost of every position, open or settled.
	FeesPaid         float64 `json:"fees_paid"`
	TotalReturn      float64 `json:"total_return"`
	OpenExposure     float64 `json:"open_exposure"`
	OpenPositions    int     `json:"open_positions"`
	SettledPositions int     `json:"settled_positions"`
	SplitPositions   int     `json:"split_positions"`
	// StoppedPositions counts positions closed early by the stop-loss. They are
	// settled for cash purposes but excluded from the win-rate denominator.
	StoppedPositions int     `json:"stopped_positions"`
	WinRate          float64 `json:"win_rate"`
	TotalOrders      int64   `json:"total_orders"`
}

type EngineStatus struct {
	Running    bool       `json:"running"`
	LastScanAt *time.Time `json:"last_scan_at"`
	LastError  string     `json:"last_error"`
	ScanCount  int64      `json:"scan_count"`
	Mode       string     `json:"mode"`
}

type SourceStatus struct {
	Name       string     `json:"name"`
	LastSyncAt *time.Time `json:"last_sync_at"`
}

type Dashboard struct {
	Account Account       `json:"account"`
	Metrics Metrics       `json:"metrics"`
	Engine  EngineStatus  `json:"engine"`
	Source  SourceStatus  `json:"source"`
	Equity  []EquityPoint `json:"equity"`
}

type OpportunityList struct {
	Items     []Opportunity `json:"items"`
	Total     int           `json:"total"`
	ScannedAt *time.Time    `json:"scanned_at"`
	Source    string        `json:"source"`
	// Venues lists the venues that contributed market data to this scan.
	Venues []market.Venue `json:"venues"`
}

// ScanOptions controls one scan pass. Execute enables automatic entries and the
// stop-loss monitor. A non-zero AccountID scopes execution to one account;
// zero means every account, which is what the background engine uses.
type ScanOptions struct {
	Execute   bool
	AccountID int64
	// Venue limits the scan to one venue. Empty means every enabled venue.
	Venue market.Venue
	// Manual marks a user-initiated run. It overrides the account's background
	// AutoTrade switch for this single pass so the radar's "自动下单" choice
	// takes effect even when background auto trading is disabled.
	Manual bool
}

// ScanSummary reports what a scan did for the requesting account. OrdersPlaced
// counts automatic entries opened by this pass; SkippedHeld counts candidate
// markets that already had a position record (open, settled or sold) and were
// therefore not bought again.
type ScanSummary struct {
	AccountID    int64  `json:"account_id"`
	Execute      bool   `json:"execute"`
	OrdersPlaced int    `json:"orders_placed"`
	SkippedHeld  int    `json:"skipped_held"`
	Error        string `json:"error,omitempty"`
}

type PositionList struct {
	Items []Position `json:"items"`
	Total int        `json:"total"`
}

type OrderList struct {
	Items []Order `json:"items"`
	Total int     `json:"total"`
}

type AccountList struct {
	Items []Account `json:"items"`
	Total int       `json:"total"`
}

// LiveFill is the real exchange execution returned by a venue executor. For a
// buy, Cost is the gross notional plus the taker fee. For a sell, Cost is the
// gross proceeds minus the taker fee. Price is always the execution price
// excluding the fee.
//
// It is an alias of venue.Fill so an executor can implement the interface
// without importing the engine (the import would be a cycle).
type LiveFill = venue.Fill

// CashSnapshot is a venue's collateral balance and allowance. The currency is
// implied by the executor that produced it (Polymarket USDC, Predict.fun USDT),
// so no amount is ever added to a snapshot from another venue.
type CashSnapshot = venue.CashSnapshot

// LiveExecutor is one venue's trading surface. The identifiers it takes are
// interpreted by that venue: the same decimal strings mean a CTF token id on
// Polymarket and a CTF onChainId on Predict.fun, and the executor receiving
// them is always the one that owns them. A missing executor means the venue is
// read-only and live accounts on it refuse orders.
type LiveExecutor = venue.Executor

// LiveQuote is a read-only executable estimate returned by a venue executor.
// LimitPrice is the worst ask level a FAK buy would touch (tick-ceiled), the
// same convention MarketBuyOrder uses when it sizes and signs the real order.
// FillPrice is the expected average execution price after walking every level.
type LiveQuote = venue.Quote

type Repository interface {
	LoadSettings(context.Context) (Settings, error)
	SaveSettings(context.Context, Settings) error

	LoadSystemSettings(context.Context) (SystemSettings, error)
	SaveSystemSettings(context.Context, SystemSettings) error

	ListAccounts(context.Context) ([]Account, error)
	AccountByID(context.Context, int64) (Account, error)
	DefaultAccount(context.Context) (Account, error)
	CreateAccount(context.Context, Account) (Account, error)
	UpdateAccount(context.Context, Account) error
	SetDefaultAccount(context.Context, int64) error
	DeleteAccount(context.Context, int64) error
	UpdateAccountBaseline(context.Context, int64, float64) error

	CreatePosition(context.Context, Position) (Position, error)
	CreatePositionWithOrder(context.Context, Position, Order) (Position, Order, error)
	CreateOrder(context.Context, Order) (Order, error)
	ListPositions(context.Context, int64, string, market.Venue) ([]Position, error)
	ListOrders(context.Context, int64, string, int, int, market.Venue) ([]Order, error)
	CountOrders(context.Context, int64, string, market.Venue) (int64, error)
	ListUnresolvedOrders(context.Context, int64) ([]Order, error)
	MarkOrderStatus(context.Context, int64, string, string) error
	FinalizeLiveBuy(context.Context, int64, Position, LiveFill) (Position, error)
	// SellPosition books a fully simulated paper sell and inserts its order.
	SellPosition(context.Context, Position, Order) error
	// FinalizeLiveSell marks a pending/unknown real sell as filled and updates
	// the position it closed or reduced.
	FinalizeLiveSell(context.Context, int64, Position, LiveFill) (Position, error)
	UpdateMark(context.Context, int64, float64, float64) error
	SettlePosition(context.Context, Position, Order) error
	AddSnapshot(context.Context, int64, EquityPoint) error
	ListSnapshots(context.Context, int64, int) ([]EquityPoint, error)
}

func opportunityFor(m market.Market, outcome market.Outcome, now time.Time) Opportunity {
	days := m.EndDate.Sub(now).Hours() / 24
	grossYield := (1 - outcome.Price) / outcome.Price
	return Opportunity{
		MarketID: m.ID, ConditionID: m.ConditionID, Question: m.Question, GroupItemTitle: m.GroupItemTitle,
		Slug: m.Slug, Outcome: outcome.Name, TokenID: outcome.TokenID, Price: outcome.Price,
		Liquidity: m.Liquidity, Volume: m.Volume, EndDate: m.EndDate, DaysToEnd: days,
		GrossYield: grossYield, URL: m.URL(), PriceChange24h: m.PriceChange24h,
		Version: m.Version, Venue: m.EffectiveVenue(), Currency: m.EffectiveCurrency(),
		FeeRateBPS: m.FeeRateBPS,
	}
}

func findOpportunity(m market.Market, outcomeName, tokenID string, settings Settings, now time.Time) (Opportunity, error) {
	if !m.Active || m.Closed || m.EndDate.IsZero() || !m.EndDate.After(now) || m.EndDate.After(now.AddDate(0, 0, settings.MaxDays)) {
		return Opportunity{}, fmt.Errorf("%w: 市场未开放或不在到期范围内", ErrInvalidOrder)
	}
	if !m.TradeReady() {
		return Opportunity{}, fmt.Errorf("%w: 市场已暂停接受订单（acceptingOrders=false）", ErrInvalidOrder)
	}
	// The floor is per venue: a book that is thin by Polymarket standards is
	// normal on Predict.fun, and one shared number would either hide every
	// Predict.fun row or let Polymarket noise through.
	if len(m.Outcomes) != 2 || m.Liquidity < settings.For(m.EffectiveVenue()).MinLiquidity {
		return Opportunity{}, fmt.Errorf("%w: 非二元市场或流动性不足", ErrInvalidOrder)
	}
	if settings.MaxPriceChange24h > 0 && m.PriceChange24h != nil &&
		math.Abs(*m.PriceChange24h) > settings.MaxPriceChange24h+1e-9 {
		return Opportunity{}, fmt.Errorf("%w: 24 小时价格变动 %.4f 超过上限 %.4f", ErrInvalidOrder, *m.PriceChange24h, settings.MaxPriceChange24h)
	}
	for _, outcome := range m.Outcomes {
		if (tokenID != "" && outcome.TokenID != tokenID) || (tokenID == "" && !strings.EqualFold(outcome.Name, outcomeName)) {
			continue
		}
		if tokenID != "" && !strings.EqualFold(outcome.Name, outcomeName) {
			return Opportunity{}, fmt.Errorf("%w: 结果与 token 不匹配", ErrInvalidOrder)
		}
		if outcome.Price < settings.MinPrice || outcome.Price > settings.MaxPrice {
			return Opportunity{}, fmt.Errorf("%w: 参考价 %.4f 不在 %.4f–%.4f 内", ErrInvalidOrder, outcome.Price, settings.MinPrice, settings.MaxPrice)
		}
		return opportunityFor(m, outcome, now), nil
	}
	return Opportunity{}, ErrNotFound
}
