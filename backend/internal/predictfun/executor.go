package predictfun

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/venue"
)

const (
	// defaultSlippageBPS is the price cushion a marketable order gets on top of
	// the visible book. Predict.fun books are thin (a single market can hold
	// under $100 of depth), so a cushion is required to fill at all; the same
	// number is the venue capability's MaxSlippageBPS.
	defaultSlippageBPS = 200.0
	// orderValiditySeconds mirrors the SDK's FIVE_MINUTES_SECONDS GTD window.
	// A signed order that is never submitted expires instead of resting.
	orderValiditySeconds = 300
	// shareStep is the granularity a computed share count is floored to. It
	// keeps an 18-decimal amount from being one wei short of the signed
	// takerAmount and lets a usably readable share count reach the UI.
	shareStep = 1e-6
	// collateralSnapshotTTL is how long a USDT balance/allowance read is reused.
	collateralSnapshotTTL = 15 * time.Second
)

// oneUSDT is 1e18 wei, the smallest order value this venue accepts.
var oneUSDT = venue.Scale(1, venue.USDTDecimals)

// Executor is the live Predict.fun trading surface. It signs each order exactly
// once and never re-signs inside a retry: the exchange deduplicates on the order
// hash, so a fresh salt on retry would become a second order.
type Executor struct {
	client   *Client
	provider *Provider
	chain    *OnChain
	chainErr error

	// signOrder is a seam so a test can count signatures. Production signs with
	// the configured key.
	signOrder func(Order, OrderDomain) (string, common.Hash, error)
	now       func() time.Time

	mu             sync.Mutex
	generation     uint64
	snapshot       venue.CashSnapshot
	snapshotAt     time.Time
	approvalKnown  bool
	approvalsReady bool
}

// Executor implements venue.Executor.
var _ venue.Executor = (*Executor)(nil)

// The engine finds these capabilities by type assertion on the registered
// executor. A signature drift still compiles and silently disables the
// feature, so the assertions below are part of the build.
var (
	_ interface {
		ApplyLiveSettings(apiKey, privateKey, smartWallet string, testnet *bool, baseURL string) error
		ResetLiveSettings() error
	} = (*Executor)(nil)
	_ interface {
		SetBaseURL(string) error
		BaseURL() string
		DefaultBaseURL() string
	} = (*Executor)(nil)
)

// ApplyLiveSettings forwards a credential update to the shared client. The
// executor is what the engine registers for trading, so it has to expose the
// data client's runtime config methods instead of hiding them.
func (e *Executor) ApplyLiveSettings(apiKey, privateKey, smartWallet string, testnet *bool, baseURL string) error {
	if e == nil || e.client == nil {
		return fmt.Errorf("Predict.fun 客户端未初始化")
	}
	return e.client.ApplyLiveSettings(apiKey, privateKey, smartWallet, testnet, baseURL)
}

// ResetLiveSettings returns the shared client to its startup configuration.
func (e *Executor) ResetLiveSettings() error {
	if e == nil || e.client == nil {
		return fmt.Errorf("Predict.fun 客户端未初始化")
	}
	return e.client.ResetLiveSettings()
}

// SetBaseURL changes the REST endpoint at runtime.
func (e *Executor) SetBaseURL(raw string) error {
	if e == nil || e.client == nil {
		return fmt.Errorf("Predict.fun 客户端未初始化")
	}
	return e.client.SetBaseURL(raw)
}

// Testnet reports the network currently in use.
func (e *Executor) Testnet() bool {
	if e == nil || e.client == nil {
		return false
	}
	return e.client.Testnet()
}

// BaseURL reports the endpoint in use.
func (e *Executor) BaseURL() string {
	if e == nil || e.client == nil {
		return ""
	}
	return e.client.BaseURL()
}

// DefaultBaseURL reports the endpoint the current network would use.
func (e *Executor) DefaultBaseURL() string {
	if e == nil || e.client == nil {
		return ""
	}
	return e.client.DefaultBaseURL()
}

// NewExecutor builds the executor around an existing client. It shares the
// provider because the executor needs the token-to-market metadata the provider
// captured while reading the market list.
func NewExecutor(client *Client, provider *Provider) *Executor {
	executor := &Executor{client: client, provider: provider, now: time.Now}
	if client == nil {
		return executor
	}
	if provider == nil {
		executor.provider = NewProvider(client)
	}
	executor.signOrder = func(order Order, domain OrderDomain) (string, common.Hash, error) {
		// Read the key at signing time, not construction time: the settings page
		// can replace the credentials while the process keeps running.
		cfg := client.current()
		if cfg.key == nil {
			return "", common.Hash{}, fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY")
		}
		// An order built from one wallet must never be signed by another, so a
		// snapshot swap between building and signing fails instead of producing
		// an order whose maker cannot move the funds.
		if cfg.signer != order.Signer || domain.ChainID.Int64() != cfg.chainID {
			return "", common.Hash{}, fmt.Errorf("Predict.fun 配置已变更，请重新下单")
		}
		return order.Sign(cfg.key, domain)
	}
	executor.rebuildChain()
	client.OnReconfigure(executor.rebuildChain)
	return executor
}

// rebuildChain re-derives the on-chain client from the current configuration so
// approvals and balance reads follow a credential or network change instead of
// using the wallet that happened to be configured at startup.
func (e *Executor) rebuildChain() {
	if e == nil || e.client == nil {
		return
	}
	chain, err := NewOnChain(e.client.current().raw)
	e.mu.Lock()
	e.chain, e.chainErr = chain, err
	e.generation++
	e.approvalKnown = false
	e.approvalsReady = false
	e.snapshotAt = time.Time{}
	e.mu.Unlock()
}

// chainNow returns the current on-chain client and its construction error.
func (e *Executor) chainNow() (*OnChain, error) {
	if e == nil || e.client == nil {
		return nil, fmt.Errorf("Predict.fun 链上配置未初始化")
	}
	e.mu.Lock()
	chain, err := e.chain, e.chainErr
	e.mu.Unlock()
	if chain == nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("Predict.fun 链上配置未初始化")
	}
	return chain, nil
}

// chainSnapshot returns the on-chain client together with the configuration
// generation it belongs to. A balance read must not cache its result if the
// wallet or network changed while the RPC call was in flight.
func (e *Executor) chainSnapshot() (*OnChain, uint64, error) {
	if e == nil || e.client == nil {
		return nil, 0, fmt.Errorf("Predict.fun 链上配置未初始化")
	}
	e.mu.Lock()
	chain, err, generation := e.chain, e.chainErr, e.generation
	e.mu.Unlock()
	if chain == nil {
		if err != nil {
			return nil, generation, err
		}
		return nil, generation, fmt.Errorf("Predict.fun 链上配置未初始化")
	}
	return chain, generation, nil
}

// New builds a provider and an executor that share one REST client. It is the
// wiring entry point for the venue registry.
func New(cfg Config) (*Provider, *Executor, error) {
	client, err := NewClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	provider := NewProvider(client)
	return provider, NewExecutor(client, provider), nil
}

// Capabilities is the fact sheet the registry and UI render. It is a function of
// the configuration rather than a constant so the testnet flag follows the
// configured network.
func Capabilities(cfg Config) venue.Capabilities {
	return venue.Capabilities{
		Venue:          market.VenuePredictFun,
		Label:          "Predict.fun",
		Currency:       market.CurrencyUSDT,
		QuoteDecimals:  venue.USDTDecimals,
		MinNotional:    defaultMinNotional,
		OrderTypes:     []string{"LIMIT", "MARKET"},
		MaxSlippageBPS: defaultSlippageBPS,
		SupportsCancel: true,
		// Settlement is readable: the provider maps the published winning outcome
		// onto a 1/0 payout, so the engine can close positions. Claiming the
		// collateral is a separate capability (SupportsRedeem).
		SupportsSettlement: true,
		// Redemption is implemented for both market shapes: plain markets through
		// ConditionalTokens.redeemPositions, neg-risk markets through the
		// NegRiskAdapter. The operator still pays BNB gas for it.
		SupportsRedeem: true,
		Testnet:        cfg.Testnet,
	}
}

// Configured reports whether both credentials a live order needs are present.
func (e *Executor) Configured() bool {
	if e == nil || e.client == nil {
		return false
	}
	return e.client.Configured()
}

// Signer is the address that signs orders and approval transactions.
func (e *Executor) Signer() string {
	if e == nil || e.client == nil {
		return ""
	}
	return e.client.Signer()
}

// Maker is the address that holds the USDT and the outcome shares.
func (e *Executor) Maker() string {
	if e == nil || e.client == nil {
		return ""
	}
	return e.client.Maker()
}

// ReadyError explains why live trading cannot run. Empty means ready.
//
// The approval clause uses the last balance/allowance snapshot because this is a
// status getter that the engine and the UI call on every render; a blocking RPC
// read here would make a status page depend on a node. Collateral refreshes the
// snapshot, so the message appears as soon as one balance read has happened.
func (e *Executor) ReadyError() string {
	if e == nil || e.client == nil {
		return "Predict.fun 实盘未配置"
	}
	if _, err := e.chainNow(); err != nil {
		return fmt.Sprintf("Predict.fun 链上配置无效：%v", err)
	}
	cfg := e.client.current()
	if cfg.key == nil {
		return "Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY"
	}
	if cfg.apiKey == "" {
		return "Predict.fun 实盘未配置：缺少 PREDICTFUN_API_KEY"
	}
	if cfg.maker != cfg.signer {
		// A smart-wallet maker needs the Kernel domain and a validator call. An
		// EOA signature over the CTF exchange domain would be valid for an order
		// whose funds the signer cannot move, so refuse instead of sending it.
		return "Predict.fun 当前只支持 EOA 模式：请清空 smart wallet（maker 必须等于 signer）"
	}
	e.mu.Lock()
	known, ready, snapshot := e.approvalKnown, e.approvalsReady, e.snapshot
	e.mu.Unlock()
	if known && snapshot.Amount > 0 && (!ready || snapshot.Allowance < defaultMinNotional) {
		return fmt.Sprintf("Predict.fun 实盘需要先授权 USDT 与份额合约（USDT 余额 %.2f，授权额度 %.2f），请执行链上授权",
			snapshot.Amount, snapshot.Allowance)
	}
	return ""
}

// EnsureApprovals sends the USDT and ERC-1155 approvals a live order needs. It
// is an explicit call: silently spending gas inside an order path would make a
// trade fail differently depending on the wallet's history.
func (e *Executor) EnsureApprovals(ctx context.Context) error {
	chain, err := e.chainNow()
	if err != nil {
		return err
	}
	if e.client.current().key == nil {
		return fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY")
	}
	if err := chain.EnsureApprovals(ctx); err != nil {
		return err
	}
	// Re-read the state instead of only dropping the timestamp: ReadyError and
	// the settings page both serve the cached snapshot, so a grant that is not
	// followed by an on-chain read would keep reporting "not approved" and the
	// order path would keep refusing until something else refreshed it.
	state, stateErr := chain.Allowance(ctx)
	e.mu.Lock()
	e.snapshotAt = time.Time{}
	if stateErr == nil {
		e.approvalKnown = true
		e.approvalsReady = state.Ready(nil)
		e.snapshot = venue.CashSnapshot{
			Amount:    unscale18(state.CollateralBalance),
			Allowance: unscale18(state.CollateralAllowance()),
			At:        e.now().UTC(),
		}
	}
	e.mu.Unlock()
	if stateErr != nil {
		return fmt.Errorf("读取链上授权状态: %w", stateErr)
	}
	return nil
}

// Approvals reads the on-chain approval state without changing anything.
func (e *Executor) Approvals(ctx context.Context) (ApprovalState, error) {
	chain, err := e.chainNow()
	if err != nil {
		return ApprovalState{}, err
	}
	return chain.Allowance(ctx)
}

// Redeem burns settled outcome shares back into USDT. It is the venue.Redeemer
// capability: deliberately separate from Buying and Selling, because claiming is
// a different permission (and a different gas bill) from trading.
func (e *Executor) Redeem(ctx context.Context, request venue.RedeemRequest) (venue.RedeemResult, error) {
	if err := e.requireLive(); err != nil {
		return venue.RedeemResult{}, err
	}
	if !(request.Shares > 0) {
		return venue.RedeemResult{}, fmt.Errorf("赎回数量必须大于 0")
	}
	meta, err := e.provider.marketMetaByID(ctx, request.MarketID)
	if err != nil {
		// Older callers and persisted requests may not carry the market id;
		// the token/condition index still works when it is warm.
		meta, err = e.provider.marketMeta(ctx, request.TokenID, request.ConditionID)
	}
	if err != nil {
		return venue.RedeemResult{}, err
	}
	indexSet, err := indexSetFor(meta, request.TokenID)
	if err != nil {
		return venue.RedeemResult{}, err
	}
	conditionID := strings.TrimSpace(meta.ConditionID)
	if conditionID == "" {
		conditionID = strings.TrimSpace(request.ConditionID)
	}
	chain, err := e.chainNow()
	if err != nil {
		return venue.RedeemResult{}, err
	}
	tx, err := chain.Redeem(ctx, RedeemRequest{
		ConditionID: conditionID,
		IndexSet:    indexSet,
		// The amount is scaled here, from the human share count the position row
		// shows, so the value that reaches the chain is the checked one.
		Shares:  venue.Scale(request.Shares, venue.USDTDecimals),
		NegRisk: meta.NegRisk,
	})
	result := venue.RedeemResult{
		Venue:  market.VenuePredictFun,
		Method: tx.Method,
	}
	if tx.Hash != (common.Hash{}) {
		result.TxHash = tx.Hash.Hex()
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// indexSetFor maps an outcome token id onto the CTF index set that redemption
// burns. The set comes from the market's own outcome order (1 = Yes, 2 = No),
// never from the outcome name, because a market may call its sides Up/Down.
func indexSetFor(meta marketMeta, tokenID string) (int, error) {
	token := strings.TrimSpace(tokenID)
	switch {
	case token != "" && token == strings.TrimSpace(meta.YesTokenID):
		return 1, nil
	case token != "" && token == strings.TrimSpace(meta.NoTokenID):
		return 2, nil
	}
	return 0, fmt.Errorf("结果 %s 不属于市场 %s", tokenID, meta.MarketID)
}

// ApprovalState reports the on-chain allowances a Predict.fun order needs. It is
// the venue-neutral view the settings page renders, and it is read-only.
func (e *Executor) ApprovalState(ctx context.Context) (venue.Approval, error) {
	state, err := e.Approvals(ctx)
	if err != nil {
		return venue.Approval{}, err
	}
	return e.approvalFrom(state), nil
}

// GrantApprovals sends the missing approvals and reports the resulting state.
// Every transaction is sent from the configured wallet, so the wallet needs BNB
// for gas before live trading can work.
func (e *Executor) GrantApprovals(ctx context.Context) (venue.Approval, error) {
	if err := e.EnsureApprovals(ctx); err != nil {
		return venue.Approval{}, err
	}
	return e.ApprovalState(ctx)
}

// approvalFrom converts the on-chain state into the UI-facing summary.
func (e *Executor) approvalFrom(state ApprovalState) venue.Approval {
	balance := venue.Unscale(state.CollateralBalance, venue.USDTDecimals)
	allowance := venue.Unscale(state.CollateralAllowance(), venue.USDTDecimals)
	sharesReady := state.SharesApproved()
	// Ready(0) would report true for a wallet that approved the share transfer
	// but never approved any USDT, which cannot place a single order. Requiring a
	// non-zero allowance matches what a buy actually needs.
	ready := sharesReady && state.CollateralAllowance().Sign() > 0
	missing := ""
	switch {
	case !sharesReady:
		missing = "尚未授权交易所划转结果份额（ERC1155 setApprovalForAll）"
	case allowance <= 0:
		missing = "尚未授权交易所扣除 USDT"
	}
	return venue.Approval{
		Venue:       market.VenuePredictFun,
		Collateral:  "USDT",
		Balance:     balance,
		Allowance:   allowance,
		SharesReady: sharesReady,
		Ready:       ready,
		Missing:     missing,
	}
}

// CancelOrder removes a resting order. Unlike the Polymarket venue, Predict.fun
// exposes cancellation, which is why Capabilities reports SupportsCancel.
func (e *Executor) CancelOrder(ctx context.Context, orderID string) error {
	if e == nil || e.client == nil {
		return fmt.Errorf("Predict.fun 实盘未配置")
	}
	cfg := e.client.current()
	if cfg.apiKey == "" || cfg.key == nil {
		return fmt.Errorf("Predict.fun 实盘未配置：撤单需要 API Key 与私钥")
	}
	return e.client.CancelOrder(ctx, orderID)
}

// Buy spends amount USDT on one outcome token.
//
// Sizing mirrors the venue's market-order semantics: the taker price comes from
// the book, a 200 bps cushion is added, the result is clamped to maxPrice, and
// the share count is floored so the signed makerAmount can never exceed the
// amount the caller approved. The order is signed once, outside any retry.
func (e *Executor) Buy(ctx context.Context, tokenID, conditionID string, amount, maxPrice float64, onSigned func(orderID string) error) (venue.Fill, error) {
	if err := e.requireLive(); err != nil {
		return venue.Fill{}, err
	}
	if strings.TrimSpace(tokenID) == "" || strings.TrimSpace(conditionID) == "" {
		return venue.Fill{}, fmt.Errorf("Predict.fun 下单需要 token 与 condition")
	}
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return venue.Fill{}, fmt.Errorf("Predict.fun 买入金额必须为有限正数")
	}
	meta, err := e.provider.marketMeta(ctx, tokenID, conditionID)
	if err != nil {
		return venue.Fill{}, err
	}
	amountWei := venue.Scale(amount, venue.USDTDecimals)
	if amountWei.Cmp(oneUSDT) < 0 {
		return venue.Fill{}, fmt.Errorf("Predict.fun 下单金额 %.6f USDT 低于最小金额 1 USDT", amount)
	}
	if meta.MinNotional > 0 && amount+1e-9 < meta.MinNotional {
		return venue.Fill{}, fmt.Errorf("Predict.fun 下单金额 %.6f USDT 低于市场最小下单金额 %.2f USDT", amount, meta.MinNotional)
	}
	if !meta.FeeRateKnown {
		return venue.Fill{}, fmt.Errorf("Predict.fun 市场 %s 缺少 feeRateBps，无法安全下单", meta.MarketID)
	}
	if meta.Tick <= 0 {
		// Without decimalPrecision there is no tick to quantize against. The
		// exchange rejects an off-tick price after the order has been signed, so
		// refuse before signing instead of guessing 0.01.
		return venue.Fill{}, fmt.Errorf("Predict.fun 市场 %s 缺少 decimalPrecision，无法量化价格，拒绝签名", meta.MarketID)
	}
	feeRate, err := feeRateBig(meta.FeeRateBps)
	if err != nil {
		return venue.Fill{}, err
	}
	if approveErr := e.checkAllowance(amount); approveErr != nil {
		return venue.Fill{}, approveErr
	}
	outcome, err := e.outcomeFor(meta, tokenID)
	if err != nil {
		return venue.Fill{}, err
	}

	translated, err := e.book(ctx, meta, outcome)
	if err != nil {
		return venue.Fill{}, err
	}
	takerPrice, ok := translated.BestAsk()
	if !ok {
		return venue.Fill{}, fmt.Errorf("Predict.fun 订单簿没有可用卖盘，无法买入 %s", meta.Question)
	}
	limit, err := buyLimit(takerPrice, maxPrice, meta.Tick, defaultSlippageBPS)
	if err != nil {
		return venue.Fill{}, err
	}
	shares := math.Floor(amount/limit/shareStep) * shareStep
	if shares <= 0 {
		return venue.Fill{}, fmt.Errorf("按限价 %.4f 计算的可买份额为 0，请提高买入金额", limit)
	}
	tokenValue, err := tokenBigInt(tokenID)
	if err != nil {
		return venue.Fill{}, err
	}
	salt, err := RandomSalt()
	if err != nil {
		return venue.Fill{}, err
	}
	cfg := e.client.current()
	order := Order{
		Salt:          salt,
		Maker:         cfg.maker,
		Signer:        cfg.signer,
		Taker:         common.Address{},
		TokenID:       tokenValue,
		MakerAmount:   amountWei,
		TakerAmount:   venue.Scale(shares, venue.USDTDecimals),
		Expiration:    big.NewInt(e.now().Add(orderValiditySeconds * time.Second).Unix()),
		Nonce:         new(big.Int),
		FeeRateBps:    feeRate,
		Side:          SideBuy,
		SignatureType: SignatureTypeEOA,
	}
	return e.submit(ctx, meta, order, limit, shares, true, onSigned)
}

// Sell exchanges shares for USDT.
//
// This path is built from the mirrored spec: the signed struct swaps maker and
// taker amounts and sets side=SELL, the limit is the best bid minus the same
// slippage cushion the buy path uses. It is covered by unit tests for the
// amounts and the signature but has NOT been exercised against the live testnet,
// which only published buy-side liquidity when this was written.
func (e *Executor) Sell(ctx context.Context, tokenID, conditionID string, shares float64, onSigned func(orderID string) error) (venue.Fill, error) {
	if err := e.requireLive(); err != nil {
		return venue.Fill{}, err
	}
	if strings.TrimSpace(tokenID) == "" || strings.TrimSpace(conditionID) == "" {
		return venue.Fill{}, fmt.Errorf("Predict.fun 卖出需要 token 与 condition")
	}
	if shares <= 0 || math.IsNaN(shares) || math.IsInf(shares, 0) {
		return venue.Fill{}, fmt.Errorf("Predict.fun 卖出份额必须为有限正数")
	}
	meta, err := e.provider.marketMeta(ctx, tokenID, conditionID)
	if err != nil {
		return venue.Fill{}, err
	}
	if !meta.FeeRateKnown {
		return venue.Fill{}, fmt.Errorf("Predict.fun 市场 %s 缺少 feeRateBps，无法安全下单", meta.MarketID)
	}
	if meta.Tick <= 0 {
		return venue.Fill{}, fmt.Errorf("Predict.fun 市场 %s 缺少 decimalPrecision，无法量化价格，拒绝签名", meta.MarketID)
	}
	feeRate, err := feeRateBig(meta.FeeRateBps)
	if err != nil {
		return venue.Fill{}, err
	}
	outcome, err := e.outcomeFor(meta, tokenID)
	if err != nil {
		return venue.Fill{}, err
	}
	translated, err := e.book(ctx, meta, outcome)
	if err != nil {
		return venue.Fill{}, err
	}
	takerPrice, ok := translated.BestBid()
	if !ok {
		return venue.Fill{}, fmt.Errorf("Predict.fun 订单簿没有可用买盘，无法卖出 %s", meta.Question)
	}
	limit, err := sellLimit(takerPrice, meta.Tick, defaultSlippageBPS)
	if err != nil {
		return venue.Fill{}, err
	}
	sharesWei := venue.Scale(shares, venue.USDTDecimals)
	if sharesWei.Sign() <= 0 {
		return venue.Fill{}, fmt.Errorf("Predict.fun 卖出份额 %.8f 低于最小精度", shares)
	}
	if meta.MinNotional > 0 && shares*limit+1e-9 < meta.MinNotional {
		return venue.Fill{}, fmt.Errorf("Predict.fun 卖出名义金额 %.6f USDT 低于最小下单金额 %.2f USDT", shares*limit, meta.MinNotional)
	}
	tokenValue, err := tokenBigInt(tokenID)
	if err != nil {
		return venue.Fill{}, err
	}
	salt, err := RandomSalt()
	if err != nil {
		return venue.Fill{}, err
	}
	sellCfg := e.client.current()
	order := Order{
		Salt:          salt,
		Maker:         sellCfg.maker,
		Signer:        sellCfg.signer,
		Taker:         common.Address{},
		TokenID:       tokenValue,
		MakerAmount:   sharesWei,
		TakerAmount:   notionalFor(sharesWei, venue.Scale(limit, venue.USDTDecimals)),
		Expiration:    big.NewInt(e.now().Add(orderValiditySeconds * time.Second).Unix()),
		Nonce:         new(big.Int),
		FeeRateBps:    feeRate,
		Side:          SideSell,
		SignatureType: SignatureTypeEOA,
	}
	return e.submit(ctx, meta, order, limit, shares, false, onSigned)
}

// submit signs once and submits that same signature. A create acknowledgement
// is only proof of acceptance, not proof of execution.
//
// The callback runs after signing and before the order leaves the process, so
// a crash can never lose an order that may have reached the exchange. When
// submission itself fails the returned fill still carries the order hash as its
// id and status "unknown": the order may be live even though the HTTP call
// failed, and a caller must reconcile instead of assuming nothing happened.
// pricePerShareWei renders a decimal price as the official 18-decimal integer
// string. Sending "0.51" where the exchange expects 510000000000000000 is a
// contract violation even though both are numeric strings.
func pricePerShareWei(price float64) (string, error) {
	if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 || price > 1 {
		return "", fmt.Errorf("Predict.fun 下单价格 %v 不在 0..1 范围内", price)
	}
	rat, ok := new(big.Rat).SetString(strconv.FormatFloat(price, 'f', -1, 64))
	if !ok {
		return "", fmt.Errorf("Predict.fun 下单价格 %v 无法精确表示", price)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	rat.Mul(rat, new(big.Rat).SetInt(scale))
	if !rat.IsInt() {
		return "", fmt.Errorf("Predict.fun 下单价格 %v 超过 18 位小数", price)
	}
	return rat.Num().String(), nil
}

func (e *Executor) submit(ctx context.Context, meta marketMeta, order Order, limit, shares float64, buy bool, onSigned func(orderID string) error) (venue.Fill, error) {
	if e.signOrder == nil {
		return venue.Fill{}, fmt.Errorf("Predict.fun 签名器未初始化")
	}
	submitCfg := e.client.current()
	domain := ExchangeDomain(submitCfg.chainID, submitCfg.addr, meta.NegRisk)
	signature, hash, err := e.signOrder(order, domain)
	if err != nil {
		return venue.Fill{}, fmt.Errorf("签名 Predict.fun 订单: %w", err)
	}
	localID := hash.Hex()
	signed := order.signedOrder(signature)
	signed.Hash = localID
	minOut := true
	pricePerShare, err := pricePerShareWei(limit)
	if err != nil {
		return venue.Fill{}, err
	}
	request := CreateOrderRequest{Data: CreateOrderData{
		Order:          &signed,
		PricePerShare:  pricePerShare,
		Strategy:       StrategyMarket,
		SlippageBps:    strconv.Itoa(defaultSlippageBPS),
		IsMinAmountOut: &minOut,
	}}
	// Persist the signed order *before* it leaves the process, exactly like the
	// Polymarket executor: an order the exchange accepts but the ledger never
	// recorded cannot be reconciled, and the next scan would buy the same market
	// again because nothing marks it as held.
	if onSigned != nil {
		if hookErr := onSigned(localID); hookErr != nil {
			return venue.Fill{}, fmt.Errorf("持久化待确认 Predict.fun 订单失败: %w", hookErr)
		}
	}
	if _, err := e.client.PlaceOrder(ctx, request); err != nil {
		// The POST may have reached the exchange even though the response was
		// lost. Reconcile the local id before reporting a failure so a live order
		// becomes a position instead of an orphan pending row.
		if recovered, ok, lookupErr := e.LookupOrder(ctx, localID, meta.ConditionID); lookupErr == nil && ok {
			recovered.OrderID = localID
			return recovered, nil
		}
		// A 4xx other than rate limiting is a definitive refusal: the exchange
		// rejected the payload and no fill can appear later. Reporting "unknown"
		// there would freeze the account's stop-loss until the reconciliation
		// window expired for an order that never existed.
		if rejectedByExchange(err) {
			return venue.Fill{OrderID: localID, Status: "rejected"},
				fmt.Errorf("提交 Predict.fun 订单被拒绝（订单 %s）: %w", localID, err)
		}
		return venue.Fill{OrderID: localID, Status: "unknown"},
			fmt.Errorf("提交 Predict.fun 订单（订单 %s，结果可能未知）: %w", localID, err)
	}
	// Acceptance is not an execution fact. The create response carries an order
	// identity but no filled quantity, price or fee, so the only honest result is
	// a zero-share unknown fill; reconciliation will replace it once matches are
	// settled. Falling back to the requested shares or limit here would book a
	// trade that may never have happened.
	return venue.Fill{OrderID: localID, Status: "unknown"}, nil
}

// rejectedByExchange reports whether the exchange answered with a definitive
// refusal (a 4xx that is not a rate limit), as opposed to a transport failure
// where the order may still have been accepted.
func rejectedByExchange(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == http.StatusTooManyRequests {
		return false
	}
	return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500
}

// fillFromOrder turns a nested order and its settled matches into the only
// kind of fill the ledger may book: a terminal, fully accounted one.
//
// amountFilled is a share total without a price or cost. It is useful for
// reconciliation only after every attributable match leg has been collected.
// The matches endpoint returns whole transactions (including other makers), so
// each match contributes exactly one leg whose hash equals this order's hash.
// If the legs do not sum exactly to amountFilled, the evidence is incomplete
// and the result stays unknown; request size and limit price are never
// substitutes for execution facts.
func (e *Executor) fillFromOrder(order OrderData, matches []MatchData, fallbackID string) venue.Fill {
	orderID := firstNonEmpty(fallbackID, order.ID)
	fill := venue.Fill{OrderID: orderID, Status: "unknown"}
	if order.Order == nil {
		return fill
	}
	inner := order.Order
	innerHash := strings.TrimSpace(inner.Hash)
	if innerHash != "" && fallbackID != "" && !strings.EqualFold(innerHash, fallbackID) {
		return fill
	}
	// The hash on the nested order is optional in the official schema. The
	// lookup path used the local hash, so it is still valid identity evidence
	// when the response leaves the field out.
	hash := firstNonEmpty(innerHash, fallbackID)
	if hash != "" {
		fill.OrderID = firstNonEmpty(fallbackID, innerHash, order.ID)
	}
	if inner.Side != uint8(SideBuy) && inner.Side != uint8(SideSell) {
		return fill
	}
	status := normalizeOrderStatus(order.Status)
	switch status {
	case "filled", "cancelled", "expired", "rejected", "invalidated":
	default:
		// OPEN can keep growing and an unknown status is not evidence. Never
		// finalize on the first positive amountFilled.
		return fill
	}
	filled, ok := parseWeiAmount(order.AmountFilled)
	if !ok {
		if status == "filled" {
			return fill
		}
		fill.Status = status
		return fill
	}
	if filled.Sign() == 0 {
		if status == "filled" {
			return fill
		}
		fill.Status = status
		return fill
	}
	if hash == "" {
		return fill
	}
	totalShares := new(big.Rat)
	totalCost := new(big.Rat)
	totalFeeShares := new(big.Rat)
	totalFeeCash := new(big.Rat)
	attributed := 0
	for _, match := range matches {
		leg, ok := attributedLeg(match, hash)
		if !ok {
			return fill
		}
		if leg == nil {
			continue
		}
		amount, ok := parseWeiAmount(leg.Amount)
		if !ok || amount.Sign() <= 0 {
			return fill
		}
		price, ok := parseDecimalRat(leg.Price)
		if !ok || price.Sign() <= 0 || price.Cmp(big.NewRat(1, 1)) > 0 {
			return fill
		}
		feeShares, feeCash, ok := feeLeg(leg.Fee, price)
		if !ok {
			return fill
		}
		shares := new(big.Rat).SetFrac(amount, oneUSDT)
		totalShares.Add(totalShares, shares)
		totalCost.Add(totalCost, new(big.Rat).Mul(shares, price))
		totalFeeShares.Add(totalFeeShares, feeShares)
		totalFeeCash.Add(totalFeeCash, feeCash)
		attributed++
	}
	if attributed == 0 {
		return fill
	}
	if totalShares.Cmp(new(big.Rat).SetFrac(filled, oneUSDT)) != 0 {
		return fill
	}
	grossShares, _ := totalShares.Float64()
	grossCost, _ := totalCost.Float64()
	price, _ := new(big.Rat).Quo(totalCost, totalShares).Float64()
	if grossShares <= 0 || grossCost <= 0 || price <= 0 || price > 1 {
		return fill
	}
	if inner.Side == uint8(SideBuy) {
		// A buy fee is charged in shares ("you receive fewer shares"), whether
		// the match reports SHARES directly or COLLATERAL that must be converted
		// at the execution price. Cost remains gross, Fee stays zero: the net
		// share count already carries the charge, and the engine also adds Fee
		// to a buy's cost.
		feeShares, _ := totalFeeShares.Float64()
		netShares := grossShares - feeShares
		if netShares <= 0 {
			return fill
		}
		fill.Shares = netShares
		fill.Cost = grossCost
		fill.Price = price
		fill.Status = "filled"
		return fill
	}
	// A sell fee is charged in quote collateral. Shares stay gross, proceeds are
	// net, and Fee stays zero because the engine would subtract it again.
	feeCash, _ := totalFeeCash.Float64()
	netCost := grossCost - feeCash
	if netCost <= 0 {
		return fill
	}
	fill.Shares = grossShares
	fill.Cost = netCost
	fill.Price = price
	fill.Status = "filled"
	return fill
}

// attributedLeg returns the one leg in a match that belongs to hash. The taker
// leg and any maker leg could not both be the same order, so that shape is
// ambiguous and rejected. A nil leg with ok=true means this match belongs to
// other orders and contributes nothing.
func attributedLeg(match MatchData, hash string) (*OrderFillData, bool) {
	taker := strings.EqualFold(strings.TrimSpace(match.Taker.Hash), hash)
	maker := -1
	for i := range match.Makers {
		if !strings.EqualFold(strings.TrimSpace(match.Makers[i].Hash), hash) {
			continue
		}
		if maker >= 0 {
			return nil, false
		}
		maker = i
	}
	if taker && maker >= 0 {
		return nil, false
	}
	if taker {
		return &match.Taker, true
	}
	if maker >= 0 {
		return &match.Makers[maker], true
	}
	return nil, true
}

func parseWeiAmount(value Number) (*big.Int, bool) {
	text := strings.TrimSpace(value.String())
	if text == "" {
		return nil, false
	}
	amount, ok := new(big.Int).SetString(text, 10)
	if !ok || amount.Sign() < 0 {
		return nil, false
	}
	return amount, true
}

func parseDecimalRat(value Number) (*big.Rat, bool) {
	text := strings.TrimSpace(value.String())
	if text == "" || strings.ContainsAny(text, "/eE") {
		return nil, false
	}
	rat, ok := new(big.Rat).SetString(text)
	if !ok || rat.Sign() < 0 {
		return nil, false
	}
	return rat, true
}

func feeLeg(fee *Fee, price *big.Rat) (shares *big.Rat, cash *big.Rat, ok bool) {
	if fee == nil {
		return nil, nil, false
	}
	amount, ok := parseWeiAmount(fee.Amount)
	if !ok {
		return nil, nil, false
	}
	value := new(big.Rat).SetFrac(amount, oneUSDT)
	switch strings.ToUpper(strings.TrimSpace(fee.Type)) {
	case "SHARES":
		return new(big.Rat).Set(value), new(big.Rat).Mul(value, price), true
	case "COLLATERAL":
		return new(big.Rat).Quo(value, price), new(big.Rat).Set(value), true
	default:
		return nil, nil, false
	}
}

// LookupOrder reconciles one order. A 404 is "not found" rather than an error,
// which is what the engine needs to decide between pending and rejected.
func (e *Executor) LookupOrder(ctx context.Context, orderID, conditionID string) (venue.Fill, bool, error) {
	if e == nil || e.client == nil {
		return venue.Fill{}, false, fmt.Errorf("Predict.fun 实盘未配置")
	}
	trimmed := strings.TrimSpace(orderID)
	if trimmed == "" {
		return venue.Fill{}, false, nil
	}
	order, err := e.client.Order(ctx, trimmed)
	if err != nil {
		if IsNotFound(err) {
			return venue.Fill{}, false, nil
		}
		return venue.Fill{}, false, err
	}
	matches, complete, err := e.client.AllMatchesForOrder(ctx, trimmed)
	if err != nil {
		return venue.Fill{OrderID: trimmed, Status: "unknown"}, true, err
	}
	if !complete {
		// A page cap or repeated cursor means the match set is not provably
		// whole. Book nothing and leave the order pending for the next lookup.
		return venue.Fill{OrderID: trimmed, Status: "unknown"}, true, nil
	}
	return e.fillFromOrder(order, matches, trimmed), true, nil
}

// EstimateBuyFee returns the taker fee for a buy of amount USDT at price, using
// the market's own feeRateBps. Predict.fun charges
// feeRateBps/10000 × min(p,1-p) × shares, so a cheap contract pays a constant
// share of its notional and a fee estimate at the wrong price is a real risk
// error.
func (e *Executor) EstimateBuyFee(ctx context.Context, conditionID string, amount, price float64) (float64, error) {
	if e == nil || e.provider == nil {
		return 0, fmt.Errorf("Predict.fun 行情未配置，无法计算手续费")
	}
	if strings.TrimSpace(conditionID) == "" {
		return 0, fmt.Errorf("缺少 condition_id，无法计算 Predict.fun 手续费")
	}
	if amount <= 0 || price <= 0 || price >= 1 || math.IsNaN(amount) || math.IsNaN(price) {
		return 0, fmt.Errorf("无法按金额 %.6f、价格 %.6f 计算 Predict.fun 手续费", amount, price)
	}
	meta, err := e.provider.marketMeta(ctx, "", conditionID)
	if err != nil {
		return 0, err
	}
	if !meta.FeeRateKnown {
		return 0, fmt.Errorf("Predict.fun 市场 %s 缺少 feeRateBps，无法计算手续费", meta.MarketID)
	}
	return e.feeFor(meta, price, amount/price), nil
}

// Quote walks the translated ask book to estimate a buy. The result is
// indicative: it uses the visible depth and the market's fee rate, and it never
// signs or submits anything.
func (e *Executor) Quote(ctx context.Context, tokenID, conditionID string, amount float64) (venue.Quote, error) {
	if e == nil || e.client == nil || e.provider == nil {
		return venue.Quote{}, fmt.Errorf("Predict.fun 实盘未配置")
	}
	if strings.TrimSpace(tokenID) == "" || strings.TrimSpace(conditionID) == "" {
		return venue.Quote{}, fmt.Errorf("token 与 condition 必须有效")
	}
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return venue.Quote{}, fmt.Errorf("买入金额必须为有限正数")
	}
	meta, err := e.provider.marketMeta(ctx, tokenID, conditionID)
	if err != nil {
		return venue.Quote{}, err
	}
	if !meta.FeeRateKnown {
		return venue.Quote{}, fmt.Errorf("Predict.fun 市场 %s 缺少 feeRateBps，无法报价", meta.MarketID)
	}
	outcome, err := e.outcomeFor(meta, tokenID)
	if err != nil {
		return venue.Quote{}, err
	}
	translated, err := e.book(ctx, meta, outcome)
	if err != nil {
		return venue.Quote{}, err
	}
	if len(translated.Asks) == 0 {
		return venue.Quote{}, fmt.Errorf("Predict.fun 订单簿没有可用卖盘")
	}
	bestAsk := translated.Asks[0].Price
	available := 0.0
	for _, level := range translated.Asks {
		available += level.Price * level.Size
	}
	budget := math.Min(amount, available)
	remaining, limitPrice := budget, bestAsk
	for _, level := range translated.Asks {
		limitPrice = level.Price
		remaining -= level.Price * level.Size
		if remaining <= 0 {
			break
		}
	}
	if meta.Tick > 0 {
		// Ceil the worst touched level to the tick: the limit must not be below
		// a level the order needs to cross.
		limitPrice = roundToTickDecimals(math.Ceil(limitPrice/meta.Tick-1e-9)*meta.Tick, meta.Tick)
	}
	if limitPrice <= 0 || limitPrice >= 1 {
		return venue.Quote{}, fmt.Errorf("盘口价格 %.6f 无法成交", limitPrice)
	}
	shares := math.Floor(budget/limitPrice/shareStep) * shareStep
	if shares <= 0 {
		return venue.Quote{}, fmt.Errorf("盘口深度不足，无法买入")
	}
	notional, unfilled, fee := 0.0, shares, 0.0
	for _, level := range translated.Asks {
		take := math.Min(unfilled, level.Size)
		if take <= 0 {
			break
		}
		notional += take * level.Price
		fee += e.feeFor(meta, level.Price, take)
		unfilled -= take
	}
	if unfilled > 1e-6 {
		return venue.Quote{}, fmt.Errorf("盘口深度不足，无法覆盖 %.6f 份", shares)
	}
	return venue.Quote{
		BestAsk:           bestAsk,
		LimitPrice:        limitPrice,
		FillPrice:         math.Round(notional/shares*1e6) / 1e6,
		Shares:            shares,
		Notional:          notional,
		Fee:               fee,
		FeeRate:           meta.FeeRateBps / 10000,
		AvailableNotional: available,
		FullyFilled:       available+1e-9 >= amount,
	}, nil
}

// Collateral reads the signer's USDT balance and the allowance granted to the
// exchange family. The snapshot is cached briefly because the engine reads it
// before every order.
func (e *Executor) Collateral(ctx context.Context) (venue.CashSnapshot, error) {
	chain, generation, err := e.chainSnapshot()
	if err != nil {
		return venue.CashSnapshot{}, err
	}
	e.mu.Lock()
	if !e.snapshotAt.IsZero() && e.now().Sub(e.snapshotAt) < collateralSnapshotTTL {
		snapshot := e.snapshot
		e.mu.Unlock()
		return snapshot, nil
	}
	cached := e.snapshot
	e.mu.Unlock()

	state, err := chain.Allowance(ctx)
	if err != nil {
		return venue.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, At: cached.At, Stale: true},
			fmt.Errorf("读取 Predict.fun USDT 余额: %w", err)
	}
	snapshot := venue.CashSnapshot{
		Amount:    unscale18(state.CollateralBalance),
		Allowance: unscale18(state.CollateralAllowance()),
		At:        e.now().UTC(),
	}
	e.mu.Lock()
	if e.generation != generation {
		e.mu.Unlock()
		return venue.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, At: cached.At, Stale: true},
			fmt.Errorf("Predict.fun 配置已变更，余额读取结果已丢弃")
	}
	e.snapshot = snapshot
	e.snapshotAt = snapshot.At
	e.approvalKnown = true
	// Ready(nil) checks the ERC-1155 operator approvals, which the single
	// allowance number in CashSnapshot cannot express. Both exchanges must be
	// approved before a neg-risk order is worth signing.
	e.approvalsReady = state.Ready(nil)
	e.mu.Unlock()
	return snapshot, nil
}

// requireLive is the credential gate every order passes before doing any work.
func (e *Executor) requireLive() error {
	if e == nil || e.client == nil {
		return fmt.Errorf("Predict.fun 实盘未配置")
	}
	cfg := e.client.current()
	if cfg.key == nil {
		return fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY")
	}
	if cfg.apiKey == "" {
		return fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_API_KEY")
	}
	if cfg.maker != cfg.signer {
		return fmt.Errorf("Predict.fun 当前只支持 EOA 模式：请清空 smart wallet（maker 必须等于 signer）")
	}
	if e.provider == nil {
		return fmt.Errorf("Predict.fun 行情未配置，无法确定市场元数据")
	}
	return nil
}

// checkAllowance refuses an order the exchange will revert for a missing USDT
// approval. It only acts on a known snapshot: before the first balance read the
// engine's own allowance check is the only guard, and this must not invent one.
func (e *Executor) checkAllowance(amount float64) error {
	e.mu.Lock()
	known, snapshot := e.approvalKnown, e.snapshot
	e.mu.Unlock()
	if known && snapshot.Allowance+1e-9 < amount {
		return fmt.Errorf("Predict.fun USDT 授权额度不足（当前 %.2f，需要 %.2f），请先执行链上授权", snapshot.Allowance, amount)
	}
	return nil
}

// outcomeFor maps a token id onto its side and rejects a token that belongs to a
// different market than the caller's condition id.
func (e *Executor) outcomeFor(meta marketMeta, tokenID string) (Outcome, error) {
	outcome, ok := meta.outcomeFor(tokenID)
	if !ok {
		return "", fmt.Errorf("token %s 不属于 Predict.fun 市场 %s（condition %s）", tokenID, meta.MarketID, meta.ConditionID)
	}
	return outcome, nil
}

// book reads and translates the orderbook for one outcome.
func (e *Executor) book(ctx context.Context, meta marketMeta, outcome Outcome) (TranslatedBook, error) {
	book, err := e.client.Orderbook(ctx, meta.MarketID)
	if err != nil {
		return TranslatedBook{}, fmt.Errorf("读取 Predict.fun 盘口: %w", err)
	}
	return BookForOutcome(book, outcome, meta.Tick), nil
}

// feeFor is the venue's taker fee model: rate × min(p,1-p) × shares. The
// min(p,1-p) term means a cheap contract pays a constant fraction of its
// notional, which is why the fee cannot be modelled as a flat percentage of
// price.
func (e *Executor) feeFor(meta marketMeta, price, shares float64) float64 {
	if price <= 0 || price >= 1 || shares <= 0 || !meta.FeeRateKnown {
		return 0
	}
	return meta.FeeRateBps / 10000 * math.Min(price, 1-price) * shares
}

// buyLimit computes the signed limit price for a marketable buy:
// min(takerPrice×(1+slippage), maxPrice), quantized down to the tick.
func buyLimit(takerPrice, maxPrice, tick, slippageBPS float64) (float64, error) {
	if takerPrice <= 0 || takerPrice >= 1 || math.IsNaN(takerPrice) {
		return 0, fmt.Errorf("Predict.fun 盘口价格 %.6f 无效", takerPrice)
	}
	limit := quantizeDown(takerPrice*(1+slippageBPS/10000), tick)
	if maxPrice > 0 {
		if limit > maxPrice {
			limit = quantizeDown(maxPrice, tick)
		}
		if limit+1e-9 < takerPrice {
			return 0, fmt.Errorf("盘口价格已超过限价：盘口 %.4f，最高买入价 %.4f", takerPrice, maxPrice)
		}
	}
	if limit <= 0 || limit >= 1 {
		return 0, fmt.Errorf("Predict.fun 买入限价 %.6f 无效", limit)
	}
	return limit, nil
}

// sellLimit computes the signed limit price for a marketable sell: the best bid
// minus the slippage cushion, quantized down so the order never bids above the
// book it was priced from.
func sellLimit(takerPrice, tick, slippageBPS float64) (float64, error) {
	if takerPrice <= 0 || takerPrice >= 1 || math.IsNaN(takerPrice) {
		return 0, fmt.Errorf("Predict.fun 盘口价格 %.6f 无效", takerPrice)
	}
	limit := quantizeDown(takerPrice*(1-slippageBPS/10000), tick)
	if limit <= 0 {
		return 0, fmt.Errorf("Predict.fun 卖出限价过低（盘口 %.4f）", takerPrice)
	}
	return limit, nil
}

// notionalFor multiplies a base-unit share count by a base-unit price with
// integer math. Scale(shares*limit) would go through float64 and sign
// 4699999999999999000 for 10 × 0.47 instead of exactly 4.7e18; the taker amount
// is part of the hash, so the drift must not happen. The division truncates,
// which understates a sell's minimum proceeds rather than overstating them.
func notionalFor(sharesWei, priceWei *big.Int) *big.Int {
	product := new(big.Int).Mul(sharesWei, priceWei)
	return product.Quo(product, oneUSDT)
}

// tokenBigInt parses the ERC-1155 token id. OnChainId is a 77-digit decimal
// string, so float64 is not an option.
func tokenBigInt(tokenID string) (*big.Int, error) {
	value, ok := new(big.Int).SetString(strings.TrimSpace(tokenID), 10)
	if !ok || value.Sign() <= 0 {
		return nil, fmt.Errorf("Predict.fun token id %s 无效", snippet(tokenID))
	}
	return value, nil
}

// feeRateBig converts the bps rate into the uint256 the order signs. A
// fractional rate means the market field is not the integer bps the struct
// expects; refuse rather than round a signed value.
func feeRateBig(rate float64) (*big.Int, error) {
	if rate < 0 || rate > 1e9 || rate != math.Trunc(rate) || math.IsNaN(rate) {
		return nil, fmt.Errorf("Predict.fun 手续费率 %.4f 不是整数 bps，无法签名订单", rate)
	}
	return big.NewInt(int64(rate)), nil
}

// normalizeOrderStatus maps the API's lifecycle words onto the vocabulary the
// engine already handles ("filled", "open", "cancelled", "expired", "rejected").
func normalizeOrderStatus(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "":
		return ""
	case "OPEN", "LIVE", "NEW", "RESTING", "PENDING", "DELAYED":
		return "open"
	case "MATCHED", "FILLED", "SUCCESS", "COMPLETED", "SETTLED":
		return "filled"
	case "PARTIAL", "PARTIALLY_FILLED", "PARTIALLY_MATCHED":
		return "partially_filled"
	case "CANCELLED", "CANCELED", "REMOVED", "REMOVAL_PENDING":
		return "cancelled"
	case "REJECTED", "FAILED", "ERROR":
		return "rejected"
	case "EXPIRED":
		return "expired"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}
