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
)

var (
	ErrInvalidOrder    = errors.New("订单不符合策略或风控条件")
	ErrNotFound        = errors.New("市场或结果不存在")
	ErrUpstream        = errors.New("公开行情不可用")
	ErrLiveUnavailable = errors.New("实盘交易不可用")
	ErrAccountNotFound = errors.New("账户不存在")
	ErrAccountConflict = errors.New("账户状态冲突")
	ErrInvalidSettings = errors.New("系统设置无效")
)

type AccountMode string

const (
	AccountModeLive  AccountMode = "live"
	AccountModePaper AccountMode = "paper"
)

func (m AccountMode) Valid() bool {
	return m == AccountModeLive || m == AccountModePaper
}

// Account is an isolated portfolio. Live accounts route orders to the real
// Polymarket CLOB; paper accounts only write local simulated positions.
type Account struct {
	ID             int64       `json:"id"`
	Name           string      `json:"name"`
	Mode           AccountMode `json:"mode"`
	IsDefault      bool        `json:"is_default"`
	AutoTrade      bool        `json:"auto_trade"`
	BaselineEquity float64     `json:"baseline_equity"`
	CreatedAt      time.Time   `json:"created_at"`

	// Live* are computed at read time and are not persisted with the account.
	LiveConfigured bool   `json:"live_configured"`
	LiveReady      bool   `json:"live_ready"`
	LiveSigner     string `json:"live_signer"`
	LiveMaker      string `json:"live_maker"`
	LiveError      string `json:"live_error"`
}

// FeeModelVersion is bumped whenever the default fee accounting changes so a
// database written by an older build can be upgraded exactly once on load.
const FeeModelVersion = 2

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
	// FeeModelVersion is managed by the server; clients must not set it.
	FeeModelVersion int `json:"fee_model_version"`
}

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

func DefaultSettings() Settings {
	return Settings{
		MinPrice: 0.95, MaxPrice: 0.99, MaxDays: 30, MinLiquidity: 10000,
		OrderSize: 100, MaxPosition: 250, MaxExposure: 1000,
		ScanIntervalSeconds: 60, SlippageBPS: 20, FeeBPS: DefaultFeeBPS,
		InitialBankroll: 10000, DefaultCategory: RadarCategoryNonSports,
		SportsPolicy: SportsPolicyExcludeGames, MaxPriceChange24h: 0.10,
		FeeModelVersion: FeeModelVersion,
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
	return SystemSettings{Proxy: netproxy.DefaultConfig()}
}

func (s SystemSettings) Validate() error {
	if err := s.Proxy.Validate(); err != nil {
		return err
	}
	if err := s.DataSource.Validate(); err != nil {
		return err
	}
	return s.Live.Validate()
}

func (s SystemSettings) Normalize() SystemSettings {
	s.Proxy = s.Proxy.Normalize()
	s.DataSource = s.DataSource.Normalize()
	s.Live = s.Live.Normalize()
	return s
}

func (s SystemSettings) Redacted() SystemSettings {
	s = s.Normalize()
	s.Proxy = s.Proxy.Redacted()
	s.Live = s.Live.Redacted()
	return s
}

func (s SystemSettings) MergeSecret(previous SystemSettings) SystemSettings {
	s = s.Normalize()
	previous = previous.Normalize()
	s.Proxy = s.Proxy.MergeSecret(previous.Proxy)
	s.Live = s.Live.MergeSecret(previous.Live)
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
// is not persisted; it is assembled from the provider and live executor.
type SystemRuntime struct {
	GammaBaseURL    string          `json:"gamma_base_url"`
	CLOBBaseURL     string          `json:"clob_base_url"`
	GammaDefault    string          `json:"gamma_default"`
	CLOBDefault     string          `json:"clob_default"`
	LiveEnvironment LiveEnvironment `json:"live_environment"`
	LiveConfigured  bool            `json:"live_configured"`
	LiveReady       bool            `json:"live_ready"`
	LiveError       string          `json:"live_error"`
	Error           string          `json:"error"`
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
}

// OpportunityQuote is a read-only, executable estimate for one radar row. It
// walks the real CLOB ask book and applies the market's real taker fee
// schedule, so NetYield is what a live FAK buy of Amount would return if the
// outcome resolves in favour. It never signs or submits an order.
type OpportunityQuote struct {
	TokenID     string  `json:"token_id"`
	ConditionID string  `json:"condition_id"`
	Amount      float64 `json:"amount"`
	BestAsk     float64 `json:"best_ask"`
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
	// AvailableNotional is the USDC depth on the ask side at quote time.
	AvailableNotional float64 `json:"available_notional"`
	FullyFilled       bool    `json:"fully_filled"`
	// ExceedsMaxPrice marks a fill price the strategy's max_price would reject.
	ExceedsMaxPrice bool      `json:"exceeds_max_price"`
	MaxPrice        float64   `json:"max_price"`
	QuotedAt        time.Time `json:"quoted_at"`
}

type Position struct {
	ID            int64      `json:"id"`
	AccountID     int64      `json:"account_id"`
	MarketID      string     `json:"market_id"`
	ConditionID   string     `json:"condition_id"`
	Question      string     `json:"question"`
	Outcome       string     `json:"outcome"`
	TokenID       string     `json:"token_id"`
	Side          string     `json:"side"`
	Kind          string     `json:"kind"`
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

type Order struct {
	ID         int64   `json:"id"`
	AccountID  int64   `json:"account_id"`
	PositionID *int64  `json:"position_id"`
	MarketID   string  `json:"market_id"`
	Question   string  `json:"question"`
	Outcome    string  `json:"outcome"`
	TokenID    string  `json:"token_id"`
	Kind       string  `json:"kind"`
	Type       string  `json:"type"`
	Amount     float64 `json:"amount"`
	Price      float64 `json:"price"`
	Shares     float64 `json:"shares"`
	// Fee is the taker fee paid on top of Amount for a filled buy. Settlement
	// (redemption) orders carry no fee: Polymarket redeems at par.
	Fee        float64   `json:"fee"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	ExternalID string    `json:"external_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type EquityPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

type Metrics struct {
	Mode            string  `json:"mode"`
	InitialBankroll float64 `json:"initial_bankroll"`
	Cash            float64 `json:"cash"`
	WalletCash      float64 `json:"wallet_cash"`
	WalletCashStale bool    `json:"wallet_cash_stale"`
	Equity          float64 `json:"equity"`
	RealizedPNL     float64 `json:"realized_pnl"`
	UnrealizedPNL   float64 `json:"unrealized_pnl"`
	TotalPNL        float64 `json:"total_pnl"`
	// FeesPaid is the sum of the buy-side taker fees already booked into the
	// cost of every position, open or settled.
	FeesPaid         float64 `json:"fees_paid"`
	TotalReturn      float64 `json:"total_return"`
	OpenExposure     float64 `json:"open_exposure"`
	OpenPositions    int     `json:"open_positions"`
	SettledPositions int     `json:"settled_positions"`
	SplitPositions   int     `json:"split_positions"`
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

// LiveFill is the real exchange execution returned by the live executor.
type LiveFill struct {
	OrderID string
	Shares  float64
	Cost    float64
	Price   float64
	Fee     float64
	Status  string
}

// CashSnapshot is the real USDC collateral balance and allowance of the live
// wallet. Both values are denominated in USDC.
type CashSnapshot struct {
	Amount    float64
	Allowance float64
	At        time.Time
	Stale     bool
}

// LiveExecutor is implemented by the Polymarket CLOB adapter. A nil executor
// means no live credentials are configured; live accounts then refuse orders.
type LiveExecutor interface {
	Configured() bool
	Signer() string
	Maker() string
	ReadyError() string
	// Buy signs an order and invokes onSigned before submitting it. The
	// callback must persist the order so a crash or timeout cannot lose a
	// real fill. conditionID is required to load the market's fee schedule.
	Buy(ctx context.Context, tokenID, conditionID string, amount, maxPrice float64, onSigned func(orderID string) error) (LiveFill, error)
	LookupOrder(ctx context.Context, orderID, conditionID string) (LiveFill, bool, error)
	EstimateBuyFee(ctx context.Context, conditionID string, amount, price float64) (float64, error)
	// Quote reads the real ask book and fee schedule without submitting an
	// order. It must work without wallet credentials.
	Quote(ctx context.Context, tokenID, conditionID string, amount float64) (LiveQuote, error)
	Collateral(ctx context.Context) (CashSnapshot, error)
}

// LiveQuote is a read-only executable estimate returned by the CLOB adapter.
// LimitPrice is the worst ask level a FAK buy would touch (tick-ceiled), the
// same convention MarketBuyOrder uses when it sizes and signs the real order.
// FillPrice is the expected average execution price after walking every level.
type LiveQuote struct {
	BestAsk           float64
	LimitPrice        float64
	FillPrice         float64
	Shares            float64
	Notional          float64
	Fee               float64
	FeeRate           float64
	AvailableNotional float64
	FullyFilled       bool
}

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
	ListPositions(context.Context, int64, string) ([]Position, error)
	ListOrders(context.Context, int64, string, int, int) ([]Order, error)
	CountOrders(context.Context, int64, string) (int64, error)
	ListUnresolvedOrders(context.Context, int64) ([]Order, error)
	MarkOrderStatus(context.Context, int64, string, string) error
	FinalizeLiveBuy(context.Context, int64, Position, LiveFill) (Position, error)
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
	}
}

func findOpportunity(m market.Market, outcomeName, tokenID string, settings Settings, now time.Time) (Opportunity, error) {
	if !m.Active || m.Closed || m.EndDate.IsZero() || !m.EndDate.After(now) || m.EndDate.After(now.AddDate(0, 0, settings.MaxDays)) {
		return Opportunity{}, fmt.Errorf("%w: 市场未开放或不在到期范围内", ErrInvalidOrder)
	}
	if len(m.Outcomes) != 2 || m.Liquidity < settings.MinLiquidity {
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
