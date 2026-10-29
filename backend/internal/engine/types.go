package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
)

var (
	ErrInvalidOrder    = errors.New("订单不符合策略或风控条件")
	ErrNotFound        = errors.New("市场或结果不存在")
	ErrUpstream        = errors.New("公开行情不可用")
	ErrLiveUnavailable = errors.New("实盘交易不可用")
	ErrAccountNotFound = errors.New("账户不存在")
	ErrAccountConflict = errors.New("账户状态冲突")
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
	LiveError      string `json:"live_error"`
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
}

func DefaultSettings() Settings {
	return Settings{
		MinPrice: 0.95, MaxPrice: 0.99, MaxDays: 30, MinLiquidity: 10000,
		OrderSize: 100, MaxPosition: 250, MaxExposure: 1000,
		ScanIntervalSeconds: 60, SlippageBPS: 20, FeeBPS: 0,
		InitialBankroll: 10000,
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
	return nil
}

type Opportunity struct {
	MarketID        string    `json:"market_id"`
	ConditionID     string    `json:"condition_id"`
	Question        string    `json:"question"`
	Slug            string    `json:"slug"`
	Outcome         string    `json:"outcome"`
	TokenID         string    `json:"token_id"`
	Price           float64   `json:"price"`
	Liquidity       float64   `json:"liquidity"`
	Volume          float64   `json:"volume"`
	EndDate         time.Time `json:"end_date"`
	DaysToEnd       float64   `json:"days_to_end"`
	GrossYield      float64   `json:"gross_yield"`
	AnnualizedYield float64   `json:"annualized_yield"`
	URL             string    `json:"url"`
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
	ID         int64     `json:"id"`
	AccountID  int64     `json:"account_id"`
	PositionID *int64    `json:"position_id"`
	MarketID   string    `json:"market_id"`
	Question   string    `json:"question"`
	Outcome    string    `json:"outcome"`
	TokenID    string    `json:"token_id"`
	Kind       string    `json:"kind"`
	Type       string    `json:"type"`
	Amount     float64   `json:"amount"`
	Price      float64   `json:"price"`
	Shares     float64   `json:"shares"`
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
	Mode             string  `json:"mode"`
	InitialBankroll  float64 `json:"initial_bankroll"`
	Cash             float64 `json:"cash"`
	WalletCash       float64 `json:"wallet_cash"`
	WalletCashStale  bool    `json:"wallet_cash_stale"`
	Equity           float64 `json:"equity"`
	RealizedPNL      float64 `json:"realized_pnl"`
	UnrealizedPNL    float64 `json:"unrealized_pnl"`
	TotalPNL         float64 `json:"total_pnl"`
	TotalReturn      float64 `json:"total_return"`
	OpenExposure     float64 `json:"open_exposure"`
	OpenPositions    int     `json:"open_positions"`
	SettledPositions int     `json:"settled_positions"`
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

// CashSnapshot is the real USDC collateral balance of the live wallet.
type CashSnapshot struct {
	Amount float64
	At     time.Time
	Stale  bool
}

// LiveExecutor is implemented by the Polymarket CLOB adapter. A nil executor
// means no live credentials are configured; live accounts then refuse orders.
type LiveExecutor interface {
	Configured() bool
	Signer() string
	ReadyError() string
	Buy(ctx context.Context, tokenID string, amount, maxPrice float64) (LiveFill, error)
	Collateral(ctx context.Context) (CashSnapshot, error)
}

type Repository interface {
	LoadSettings(context.Context) (Settings, error)
	SaveSettings(context.Context, Settings) error

	ListAccounts(context.Context) ([]Account, error)
	AccountByID(context.Context, int64) (Account, error)
	DefaultAccount(context.Context) (Account, error)
	CreateAccount(context.Context, Account) (Account, error)
	UpdateAccount(context.Context, Account) error
	SetDefaultAccount(context.Context, int64) error
	DeleteAccount(context.Context, int64) error
	UpdateAccountBaseline(context.Context, int64, float64) error

	CreatePosition(context.Context, Position) (Position, error)
	CreateOrder(context.Context, Order) (Order, error)
	ListPositions(context.Context, int64, string) ([]Position, error)
	ListOrders(context.Context, int64, int) ([]Order, error)
	CountOrders(context.Context, int64) (int64, error)
	UpdateMark(context.Context, int64, float64, float64) error
	SettlePosition(context.Context, Position, Order) error
	AddSnapshot(context.Context, int64, EquityPoint) error
	ListSnapshots(context.Context, int64, int) ([]EquityPoint, error)
}

func opportunityFor(m market.Market, outcome market.Outcome, now time.Time) Opportunity {
	days := m.EndDate.Sub(now).Hours() / 24
	grossYield := (1 - outcome.Price) / outcome.Price
	annualizedDays := math.Max(days, 1.0/24)
	return Opportunity{
		MarketID: m.ID, ConditionID: m.ConditionID, Question: m.Question, Slug: m.Slug,
		Outcome: outcome.Name, TokenID: outcome.TokenID, Price: outcome.Price,
		Liquidity: m.Liquidity, Volume: m.Volume, EndDate: m.EndDate, DaysToEnd: days,
		GrossYield: grossYield, AnnualizedYield: grossYield * 365 / annualizedDays, URL: m.URL(),
	}
}

func findOpportunity(m market.Market, outcomeName, tokenID string, settings Settings, now time.Time) (Opportunity, error) {
	if !m.Active || m.Closed || m.EndDate.IsZero() || !m.EndDate.After(now) || m.EndDate.After(now.AddDate(0, 0, settings.MaxDays)) {
		return Opportunity{}, fmt.Errorf("%w: 市场未开放或不在到期范围内", ErrInvalidOrder)
	}
	if len(m.Outcomes) != 2 || m.Liquidity < settings.MinLiquidity {
		return Opportunity{}, fmt.Errorf("%w: 非二元市场或流动性不足", ErrInvalidOrder)
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
