// Package venue holds the thin structural contracts the trading engine uses to
// talk to more than one exchange.
//
// It is deliberately small: the engine, the Polymarket CLOB executor and the
// Predict.fun executor already share one shape, so this package only names that
// shape and lets the engine look implementations up by venue. Business logic
// (scoring, sizing, risk, settlement) stays in the engine; nothing in this
// package knows how a market is scored or how big an order may be.
//
// The value types (Fill, Quote, CashSnapshot) live here instead of in the
// engine so a venue executor can satisfy the interface without importing the
// engine, which would be an import cycle. The engine re-exports them as type
// aliases (engine.LiveFill = venue.Fill), so existing call sites and tests keep
// compiling unchanged.
package venue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"pennyharvester/backend/internal/market"
)

// Fill is the real exchange execution returned by a venue executor. Cost is
// always the gross notional (shares × price) with no fee folded in, and Fee is
// reported separately, because different venues book fees differently and the
// engine has exactly one place where the two are combined. Price is the
// execution price excluding the fee.
type Fill struct {
	OrderID string
	Shares  float64
	Cost    float64
	Price   float64
	Fee     float64
	Status  string
}

// CashSnapshot is the venue's spendable collateral balance. Allowance is the
// amount the exchange contract is already approved to pull; a venue that cannot
// report it leaves it zero and sets the executor's ReadyError instead.
type CashSnapshot struct {
	Amount    float64
	Allowance float64
	At        time.Time
	Stale     bool
}

// Quote is a pre-trade estimate for a buy. AvailableNotional is how much the
// venue's book can absorb at LimitPrice; FullyFilled reports whether the
// requested size fits inside it.
type Quote struct {
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

// Capabilities is the per-venue fact sheet the API and UI render. It is
// registered once per venue at startup so no caller has to guess a currency,
// a tick size or whether cancellation exists.
type Capabilities struct {
	Venue    market.Venue    `json:"venue"`
	Label    string          `json:"label"`
	Currency market.Currency `json:"currency"`
	// QuoteDecimals is how many decimals the venue's collateral token uses. A
	// 6 means "amounts are USDC-sized", an 18 means every amount crossing the
	// API boundary must be scaled by 1e18 before signing.
	QuoteDecimals int `json:"quote_decimals"`
	// MinNotional is the smallest order value, in Currency units, the venue
	// accepts. Zero means "not enforced by this build".
	MinNotional float64 `json:"min_notional"`
	// OrderTypes lists what can be submitted, in the venue's own vocabulary.
	OrderTypes []string `json:"order_types"`
	// MaxSlippageBPS is the largest deviation from the quoted price this build
	// will accept. Zero means the executor uses the settings default.
	MaxSlippageBPS float64 `json:"max_slippage_bps"`
	SupportsCancel bool    `json:"supports_cancel"`
	// SupportsSettlement reports whether the engine can settle positions from
	// reference data. Predict.fun publishes the winning outcome, so its
	// positions close automatically; Polymarket resolves through its own API.
	SupportsSettlement bool `json:"supports_settlement"`
	// SupportsRedeem reports whether the venue can burn settled shares back into
	// collateral on chain (the venue.Redeemer capability). False for Polymarket,
	// whose claims go through a relayer this build does not speak to.
	SupportsRedeem bool `json:"supports_redeem"`
	// Testnet is true when the executor is pointed at a test network.
	Testnet bool `json:"testnet,omitempty"`
	// ReadOnly is derived from the registration: a venue with market data but no
	// executor can be watched, never traded.
	ReadOnly bool `json:"read_only,omitempty"`
}

// MarketData is a provider plus the venue it serves. market.GammaClient and the
// Predict.fun provider both satisfy it; market.Provider alone does not, which
// is what forces the registry to be explicit about which venue a provider is.
type MarketData interface {
	Venue() market.Venue
	ListOpen(ctx context.Context, maxDays int) ([]market.Market, error)
	Get(ctx context.Context, id string) (market.Market, error)
	Name() string
}

// Executor is one venue's trading surface. The identifiers it takes are
// interpreted by that venue, which is why the same strings mean a CTF token id
// on Polymarket and a CTF onChainId on Predict.fun: the executor that receives
// them is always the one that owns them.
type Executor interface {
	// Configured reports whether credentials were supplied at all.
	Configured() bool
	// Signer is the address that signs orders; Maker is the address that holds
	// the funds. They differ for proxy and smart-wallet accounts.
	Signer() string
	Maker() string
	// ReadyError explains why live trading cannot run. Empty means ready.
	ReadyError() string

	// Buy spends amount units of collateral. onSigned is called with the venue
	// order id as soon as the order is submitted, so a crash between submit and
	// fill still leaves a reconcilable record.
	Buy(ctx context.Context, tokenID, conditionID string, amount, maxPrice float64, onSigned func(orderID string) error) (Fill, error)
	Sell(ctx context.Context, tokenID, conditionID string, shares float64, onSigned func(orderID string) error) (Fill, error)
	LookupOrder(ctx context.Context, orderID, conditionID string) (Fill, bool, error)
	EstimateBuyFee(ctx context.Context, conditionID string, amount, price float64) (float64, error)
	Quote(ctx context.Context, tokenID, conditionID string, amount float64) (Quote, error)
	Collateral(ctx context.Context) (CashSnapshot, error)
}

// Registry is the read-mostly map from venue to its data source and executor.
// It is built once during startup and only read afterwards, so the lock exists
// for the HTTP handlers that enumerate it, not for concurrent registration.
type Registry struct {
	mu    sync.RWMutex
	caps  map[market.Venue]Capabilities
	data  map[market.Venue]MarketData
	execs map[market.Venue]Executor
	order []market.Venue
}

func NewRegistry() *Registry {
	return &Registry{
		caps:  map[market.Venue]Capabilities{},
		data:  map[market.Venue]MarketData{},
		execs: map[market.Venue]Executor{},
	}
}

// Register adds one venue. data is required; exec may be nil for a venue this
// build can only read from. A mismatch between caps.Venue and data.Venue() is
// an error rather than a silent route to the wrong exchange.
func (r *Registry) Register(caps Capabilities, data MarketData, exec Executor) error {
	if !caps.Venue.Valid() {
		return fmt.Errorf("venue: unknown venue %q", caps.Venue)
	}
	if data == nil {
		return fmt.Errorf("venue: %s has no market data source", caps.Venue)
	}
	if got := data.Venue(); got != caps.Venue {
		return fmt.Errorf("venue: %s registered with a %s data source", caps.Venue, got)
	}
	if caps.Currency == "" {
		caps.Currency = defaultCurrency(caps.Venue)
	}
	if caps.Label == "" {
		caps.Label = caps.Venue.Label()
	}
	// A venue registered without an executor can only read markets. Deriving the
	// flag from the registration keeps it honest: every consumer that renders or
	// checks ReadOnly sees the same answer as ExecutorFor does.
	caps.ReadOnly = exec == nil
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.data[caps.Venue]; exists {
		return fmt.Errorf("venue: %s is already registered", caps.Venue)
	}
	r.caps[caps.Venue] = caps
	r.data[caps.Venue] = data
	if exec != nil {
		r.execs[caps.Venue] = exec
	}
	r.order = append(r.order, caps.Venue)
	sort.SliceStable(r.order, func(i, j int) bool {
		return venueRank(r.order[i]) < venueRank(r.order[j])
	})
	return nil
}

// Venues lists the registered venues in presentation order.
func (r *Registry) Venues() []market.Venue {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]market.Venue, len(r.order))
	copy(out, r.order)
	return out
}

func (r *Registry) Data(v market.Venue) MarketData {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.data[v]
}

func (r *Registry) Executor(v market.Venue) Executor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.execs[v]
}

// Capabilities returns the registered fact sheet for one venue. An unknown
// venue yields the zero value plus false so callers can 404 instead of
// inventing a currency.
func (r *Registry) Capabilities(v market.Venue) (Capabilities, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	caps, ok := r.caps[v]
	return caps, ok
}

// All returns every registered fact sheet in presentation order.
func (r *Registry) All() []Capabilities {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Capabilities, 0, len(r.order))
	for _, v := range r.order {
		out = append(out, r.caps[v])
	}
	return out
}

// ErrNoExecutor is returned when a venue is registered read-only but a caller
// tries to trade on it.
var ErrNoExecutor = errors.New("venue: trading is not available for this venue")

// ExecutorFor is Executor plus the error a trading call needs. Handlers use it
// so a read-only venue fails with a clear message instead of a nil panic.
func (r *Registry) ExecutorFor(v market.Venue) (Executor, error) {
	if exec := r.Executor(v); exec != nil {
		return exec, nil
	}
	if _, ok := r.Capabilities(v); !ok {
		return nil, fmt.Errorf("venue: unknown venue %q", v)
	}
	return nil, fmt.Errorf("%w: %s", ErrNoExecutor, v)
}

func venueRank(v market.Venue) int {
	for i, known := range market.VenueOrder {
		if known == v {
			return i
		}
	}
	return len(market.VenueOrder)
}

func defaultCurrency(v market.Venue) market.Currency {
	if v == market.VenuePredictFun {
		return market.CurrencyUSDT
	}
	return market.CurrencyUSDC
}
