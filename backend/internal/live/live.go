// Package live is the real-money execution adapter. It only places orders
// through Polymarket's CLOB when a private key is explicitly configured.
package live

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/netproxy"
)

type Config struct {
	PrivateKey    string
	Funder        string
	SignatureType int
	CLOBURL       string
	APIKey        string
	APISecret     string
	APIPassphrase string
	// BuilderCode attributes every signed order to a Polymarket builder
	// profile. It is public and may be empty (unattributed orders).
	BuilderCode string
	// Proxy is the outbound egress shared with the market data client. The
	// executor rebuilds its CLOB client when the system settings change.
	Proxy *netproxy.Manager
}

type Executor struct {
	// base is the immutable startup configuration from the process environment.
	// cfg is the effective configuration after web-managed overrides.
	base Config
	cfg  Config

	mu          sync.Mutex
	credMu      sync.Mutex
	client      *clobclient.Client
	initErr     error
	credentials *clobclient.Credentials
	cash        engine.CashSnapshot
	cashAt      time.Time
	signer      string
	builderCode string
	builderErr  error
}

func New(cfg Config) *Executor {
	cfg.PrivateKey = normalizeKeyPrefix(strings.TrimSpace(cfg.PrivateKey))
	cfg.Funder = normalizeKeyPrefix(strings.TrimSpace(cfg.Funder))
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.APISecret = strings.TrimSpace(cfg.APISecret)
	cfg.APIPassphrase = strings.TrimSpace(cfg.APIPassphrase)
	cfg.CLOBURL = strings.TrimRight(strings.TrimSpace(cfg.CLOBURL), "/")
	builderCode, builderErr := normalizeBuilderCode(cfg.BuilderCode)
	return &Executor{base: cfg, cfg: cfg, builderCode: builderCode, builderErr: builderErr}
}

func normalizeKeyPrefix(value string) string {
	if len(value) >= 2 && strings.EqualFold(value[:2], "0x") {
		return "0x" + value[2:]
	}
	return value
}

func (e *Executor) Configured() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.configuredLocked()
}

func (e *Executor) configuredLocked() bool {
	return strings.TrimSpace(e.cfg.PrivateKey) != ""
}

func (e *Executor) Signer() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureClientLocked()
	return e.signer
}

// Maker returns the wallet that owns the CLOB position. For EOA accounts it is
// the signer; for proxy, Safe and deposit wallets it is POLYMARKET_FUNDER.
func (e *Executor) Maker() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureClientLocked()
	if e.client == nil {
		return ""
	}
	return e.client.MakerAddress()
}

func (e *Executor) ReadyError() string {
	if e == nil {
		return "未配置实盘执行器"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.configuredLocked() {
		return "未配置 POLYMARKET_PRIVATE_KEY"
	}
	e.ensureClientLocked()
	if e.initErr != nil {
		return e.initErr.Error()
	}
	return ""
}

// UpdateSettings applies web-managed overrides on top of the startup
// environment. Empty override fields inherit the environment. The new
// configuration is validated before it replaces the live client, so a bad
// private key or funder never breaks a working executor.
func (e *Executor) UpdateSettings(settings engine.LiveSettings, clobBaseURL string) error {
	if e == nil {
		return nil
	}
	settings = settings.Normalize()
	e.mu.Lock()
	defer e.mu.Unlock()
	effective := e.base
	effective.Proxy = e.cfg.Proxy
	if clob := strings.TrimRight(strings.TrimSpace(clobBaseURL), "/"); clob != "" {
		effective.CLOBURL = clob
	}
	if settings.PrivateKey != "" {
		effective.PrivateKey = settings.PrivateKey
	}
	if settings.Funder != "" {
		effective.Funder = settings.Funder
	}
	if settings.SignatureType != nil {
		effective.SignatureType = *settings.SignatureType
	}
	credentialCount := 0
	for _, value := range []string{settings.APIKey, settings.APISecret, settings.APIPassphrase} {
		if strings.TrimSpace(value) != "" {
			credentialCount++
		}
	}
	if credentialCount != 0 && credentialCount != 3 {
		return fmt.Errorf("POLYMARKET_API_KEY、POLYMARKET_API_SECRET、POLYMARKET_API_PASSPHRASE 必须同时设置")
	}
	if credentialCount == 3 {
		effective.APIKey = settings.APIKey
		effective.APISecret = settings.APISecret
		effective.APIPassphrase = settings.APIPassphrase
	}
	if err := validateEffectiveConfig(effective, e.builderErr); err != nil {
		return err
	}
	if sameLiveConfig(e.cfg, effective) {
		return nil
	}
	e.cfg = effective
	e.client = nil
	e.initErr = nil
	e.credentials = nil
	e.cash = engine.CashSnapshot{}
	e.cashAt = time.Time{}
	return nil
}

// EffectiveCLOBURL returns the CLOB endpoint currently used by the executor.
func (e *Executor) EffectiveCLOBURL() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.CLOBURL
}

// Environment reports the startup values so the UI can show which fields are
// inherited from the process environment.
func (e *Executor) Environment() engine.LiveEnvironment {
	if e == nil {
		return engine.LiveEnvironment{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	base := e.base
	return engine.LiveEnvironment{
		CLOBBaseURL:      base.CLOBURL,
		PrivateKeySet:    strings.TrimSpace(base.PrivateKey) != "",
		Funder:           base.Funder,
		SignatureType:    base.SignatureType,
		APIKeySet:        strings.TrimSpace(base.APIKey) != "",
		APISecretSet:     strings.TrimSpace(base.APISecret) != "",
		APIPassphraseSet: strings.TrimSpace(base.APIPassphrase) != "",
	}
}

func sameLiveConfig(a, b Config) bool {
	return strings.TrimSpace(a.PrivateKey) == strings.TrimSpace(b.PrivateKey) &&
		strings.TrimSpace(a.Funder) == strings.TrimSpace(b.Funder) &&
		a.SignatureType == b.SignatureType &&
		strings.TrimRight(strings.TrimSpace(a.CLOBURL), "/") == strings.TrimRight(strings.TrimSpace(b.CLOBURL), "/") &&
		a.APIKey == b.APIKey && a.APISecret == b.APISecret && a.APIPassphrase == b.APIPassphrase
}

func validateEffectiveConfig(cfg Config, builderErr error) error {
	if builderErr != nil {
		return builderErr
	}
	key := strings.TrimSpace(cfg.PrivateKey)
	if key != "" {
		if !strings.HasPrefix(strings.ToLower(key), "0x") {
			return fmt.Errorf("POLYMARKET_PRIVATE_KEY 必须以 0x 开头")
		}
		if _, err := crypto.HexToECDSA(strings.TrimPrefix(key, "0x")); err != nil {
			return fmt.Errorf("POLYMARKET_PRIVATE_KEY 无效: %w", err)
		}
	}
	if cfg.SignatureType < 0 || cfg.SignatureType > 3 {
		return fmt.Errorf("POLYMARKET_SIGNATURE_TYPE 仅支持 0、1、2、3")
	}
	funder := strings.TrimSpace(cfg.Funder)
	if funder != "" && !common.IsHexAddress(funder) {
		return fmt.Errorf("POLYMARKET_FUNDER 不是有效地址")
	}
	if cfg.SignatureType == 0 {
		if funder != "" {
			return fmt.Errorf("EOA 签名类型不需要 POLYMARKET_FUNDER；请留空，或改用 1/2/3")
		}
	} else if key != "" && funder == "" {
		return fmt.Errorf("非 EOA 签名类型需要设置 POLYMARKET_FUNDER（代理钱包地址）")
	}
	credentialCount := 0
	for _, value := range []string{cfg.APIKey, cfg.APISecret, cfg.APIPassphrase} {
		if strings.TrimSpace(value) != "" {
			credentialCount++
		}
	}
	if credentialCount != 0 && credentialCount != 3 {
		return fmt.Errorf("POLYMARKET_API_KEY、POLYMARKET_API_SECRET、POLYMARKET_API_PASSPHRASE 必须同时设置")
	}
	if clob := strings.TrimSpace(cfg.CLOBURL); clob != "" {
		parsed, err := url.Parse(clob)
		if err != nil {
			return fmt.Errorf("POLYMARKET_CLOB_URL 无效: %w", err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("POLYMARKET_CLOB_URL 仅支持 http 或 https")
		}
		if parsed.Host == "" {
			return fmt.Errorf("POLYMARKET_CLOB_URL 缺少主机与端口")
		}
	}
	return nil
}

func (e *Executor) ensureClientLocked() {
	if e.client != nil || e.initErr != nil {
		return
	}
	if !e.configuredLocked() {
		e.initErr = fmt.Errorf("未配置 POLYMARKET_PRIVATE_KEY")
		return
	}
	if err := validateEffectiveConfig(e.cfg, e.builderErr); err != nil {
		e.initErr = err
		return
	}
	funder := strings.TrimSpace(e.cfg.Funder)
	credentialCount := 0
	for _, value := range []string{e.cfg.APIKey, e.cfg.APISecret, e.cfg.APIPassphrase} {
		if strings.TrimSpace(value) != "" {
			credentialCount++
		}
	}
	makerAddress := ""
	if e.cfg.SignatureType != 0 {
		makerAddress = funder
	}
	cfg := clobclient.Config{
		PrivateKey:    strings.TrimSpace(e.cfg.PrivateKey),
		MakerAddress:  makerAddress,
		SignatureType: clobclient.SignatureType(e.cfg.SignatureType),
		Host:          e.cfg.CLOBURL,
		Timeout:       20 * time.Second,
		QPS:           10,
	}
	if credentialCount == 3 {
		cfg.Credentials = &clobclient.Credentials{
			APIKey: e.cfg.APIKey, Secret: e.cfg.APISecret, Passphrase: e.cfg.APIPassphrase,
		}
	} else if e.credentials != nil {
		// Reuse credentials derived before the client was rebuilt for a new
		// proxy, instead of hitting the CLOB credential endpoint again.
		cfg.Credentials = e.credentials
	}
	if e.cfg.Proxy != nil {
		if e.cfg.Proxy.Config().Mode == netproxy.ModeOff {
			// The SDK has no "ignore the environment" switch, so it gets a
			// prebuilt client that connects directly. Reads and orders then
			// share one pool, which only happens in this opt-out mode.
			cfg.HTTPClient = e.cfg.Proxy.HTTPClient(20 * time.Second)
		} else {
			cfg.ProxyURL = e.cfg.Proxy.ProxyURL()
		}
	}
	client, err := clobclient.New(cfg)
	if err != nil {
		e.initErr = fmt.Errorf("初始化 Polymarket CLOB 客户端: %w", err)
		return
	}
	e.client = client
	e.signer = client.Address()
}

// ReloadProxy drops the cached CLOB client so the next call rebuilds its HTTP
// transports from the current outbound proxy. Derived L2 credentials survive
// the rebuild so no extra credential round-trip is needed.
func (e *Executor) ReloadProxy() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.client = nil
	e.initErr = nil
	e.mu.Unlock()
}

func (e *Executor) ensure(ctx context.Context) (*clobclient.Client, error) {
	e.mu.Lock()
	e.ensureClientLocked()
	client := e.client
	initErr := e.initErr
	cfg := e.cfg
	e.mu.Unlock()
	if initErr != nil {
		return nil, initErr
	}
	// Credential derivation is idempotent but does network I/O. Serialize it
	// separately so a transient failure can be retried on the next order.
	e.credMu.Lock()
	defer e.credMu.Unlock()
	credentials, err := client.EnsureCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("派生 Polymarket CLOB API 凭据: %w", err)
	}
	if credentials != nil {
		e.mu.Lock()
		// Do not cache credentials derived for a client that was replaced by a
		// concurrent settings update; they may belong to a different wallet or
		// host and would otherwise be reused by the next client.
		if sameLiveConfig(e.cfg, cfg) {
			e.credentials = credentials
		}
		e.mu.Unlock()
	}
	return client, nil
}

func (e *Executor) Buy(ctx context.Context, tokenID, conditionID string, amount, maxPrice float64, onSigned func(orderID string) error) (engine.LiveFill, error) {
	if tokenID == "" || conditionID == "" || amount <= 0 {
		return engine.LiveFill{}, fmt.Errorf("%w: token、condition 与金额必须有效", engine.ErrInvalidOrder)
	}
	client, err := e.ensure(ctx)
	if err != nil {
		return engine.LiveFill{}, err
	}
	schedule, err := client.FeeSchedule(ctx, conditionID)
	if err != nil {
		return engine.LiveFill{}, fmt.Errorf("读取实盘手续费: %w", err)
	}
	order, err := client.MarketBuyOrder(ctx, tokenID, amount, clobclient.OrderTypeFAK)
	if err != nil {
		return engine.LiveFill{}, fmt.Errorf("构建实盘买单: %w", err)
	}
	// The builder code is serialized into the signed order, so it has to be set
	// before signing; it is what attributes the matched trade to the builder.
	order.Builder = e.builderCode
	if maxPrice > 0 && order.Price > maxPrice+1e-9 {
		return engine.LiveFill{}, fmt.Errorf("%w: 盘口成交价 %.6f 超过最高买入价 %.6f", engine.ErrInvalidOrder, order.Price, maxPrice)
	}
	signed, err := client.CreateOrder(ctx, order)
	if err != nil {
		return engine.LiveFill{}, fmt.Errorf("签名实盘买单: %w", err)
	}
	if onSigned != nil {
		if err := onSigned(signed.OrderID); err != nil {
			return engine.LiveFill{}, fmt.Errorf("持久化待确认实盘订单: %w", err)
		}
	}
	response, submitErr := client.SubmitSignedOrder(ctx, signed, clobclient.OrderTypeFAK, false)
	if submitErr != nil {
		// A timeout can still mean the exchange accepted the order. Look the
		// signed digest up before reporting a rejection so a real fill is not
		// silently lost from the local ledger.
		if detail, lookupErr := client.Order(ctx, signed.OrderID); lookupErr == nil {
			if fill, ok := fillFromOrder(detail, order.Price); ok {
				if feeErr := applyFee(schedule, &fill); feeErr != nil {
					return engine.LiveFill{OrderID: signed.OrderID, Status: "unknown"}, feeErr
				}
				return fill, nil
			}
		}
		return engine.LiveFill{OrderID: signed.OrderID, Status: "unknown"}, fmt.Errorf("提交实盘买单（订单 %s，结果可能未知）: %w", signed.OrderID, submitErr)
	}
	if response == nil {
		return engine.LiveFill{OrderID: signed.OrderID, Status: "unknown"}, fmt.Errorf("Polymarket 返回空订单响应")
	}
	if !response.Success {
		message := strings.TrimSpace(response.ErrorMsg)
		if message == "" {
			message = "Polymarket 拒绝了订单"
		}
		if response.OrderID != "" {
			message = fmt.Sprintf("%s（订单 %s）", message, response.OrderID)
		}
		return engine.LiveFill{OrderID: response.OrderID, Status: "rejected"}, fmt.Errorf("%s", message)
	}
	fill := engine.LiveFill{OrderID: response.OrderID, Status: response.Status}
	fill.Cost = parseAmount(response.MakingAmount)
	fill.Shares = parseAmount(response.TakingAmount)
	if fill.Shares <= 0 || fill.Cost <= 0 {
		if response.OrderID != "" {
			if detail, lookupErr := client.Order(ctx, response.OrderID); lookupErr == nil {
				if lookedUp, ok := fillFromOrder(detail, order.Price); ok {
					lookedUp.OrderID = response.OrderID
					if feeErr := applyFee(schedule, &lookedUp); feeErr != nil {
						return engine.LiveFill{OrderID: response.OrderID, Status: "unknown"}, feeErr
					}
					return lookedUp, nil
				}
			}
		}
	}
	if fill.Shares <= 0 || fill.Cost <= 0 {
		// The exchange accepted the order but the response did not include a
		// usable match. Treat it as unknown and reconcile instead of
		// declaring a possible fill rejected.
		originalStatus := fill.Status
		if fill.OrderID == "" {
			fill.OrderID = signed.OrderID
		}
		fill.Status = "unknown"
		return fill, fmt.Errorf("实盘订单未产生可确认成交（原状态 %s）", originalStatus)
	}
	fill.Price = fill.Cost / fill.Shares
	if err := applyFee(schedule, &fill); err != nil {
		return engine.LiveFill{OrderID: fill.OrderID, Status: "unknown"}, err
	}
	return fill, nil
}

// EstimateBuyFee returns the taker fee the exchange would charge for the
// requested notional at the reference price. It is used for pre-trade cash and
// exposure checks; the actual fee is recomputed from the fill.
func (e *Executor) EstimateBuyFee(ctx context.Context, conditionID string, amount, price float64) (float64, error) {
	if conditionID == "" {
		return 0, fmt.Errorf("缺少 condition_id，无法计算实盘手续费")
	}
	if amount <= 0 || price <= 0 || price >= 1 {
		return 0, fmt.Errorf("无法按金额 %.6f、价格 %.6f 计算实盘手续费", amount, price)
	}
	client, err := e.ensure(ctx)
	if err != nil {
		return 0, err
	}
	schedule, err := client.FeeSchedule(ctx, conditionID)
	if err != nil {
		return 0, err
	}
	return feeFromSchedule(schedule, amount/price, price)
}

// LookupOrder reconciles an order by its signed digest / exchange order ID.
func (e *Executor) LookupOrder(ctx context.Context, orderID, conditionID string) (engine.LiveFill, bool, error) {
	if orderID == "" {
		return engine.LiveFill{}, false, nil
	}
	client, err := e.ensure(ctx)
	if err != nil {
		return engine.LiveFill{}, false, err
	}
	detail, err := client.Order(ctx, orderID)
	if err != nil {
		var apiErr *clobclient.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return engine.LiveFill{}, false, nil
		}
		return engine.LiveFill{}, false, err
	}
	if detail == nil {
		return engine.LiveFill{}, false, nil
	}
	fill, ok := fillFromOrder(detail, 0)
	if !ok {
		return engine.LiveFill{OrderID: orderID, Status: detail.Status}, true, nil
	}
	if fill.OrderID == "" {
		fill.OrderID = orderID
	}
	if conditionID != "" {
		schedule, scheduleErr := client.FeeSchedule(ctx, conditionID)
		if scheduleErr != nil {
			return engine.LiveFill{}, false, fmt.Errorf("对账时读取实盘手续费: %w", scheduleErr)
		}
		if feeErr := applyFee(schedule, &fill); feeErr != nil {
			return engine.LiveFill{}, false, feeErr
		}
	}
	return fill, true, nil
}

// maxFeeRate bounds the taker rate the exchange is allowed to report. The
// documented categories top out at 0.07; anything higher means the schedule is
// being read wrong, and silently booking a smaller fee would overstate PnL.
const maxFeeRate = 0.10

func feeFromSchedule(schedule clobclient.FeeSchedule, shares, price float64) (float64, error) {
	if raw := strings.TrimSpace(schedule.Rate.String()); raw != "" {
		rate, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("解析手续费率 %q: %w", raw, err)
		}
		if rate < 0 || rate > maxFeeRate {
			return 0, fmt.Errorf("手续费率 %q 超出 0–%.2f 范围", raw, maxFeeRate)
		}
	}
	raw, err := schedule.Fee(formatFloat(shares), formatFloat(price), true)
	if err != nil {
		return 0, err
	}
	fee, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("解析手续费 %q: %w", raw, err)
	}
	if fee < 0 {
		return 0, fmt.Errorf("手续费不能为负: %q", raw)
	}
	// fee = shares × rate × p × (1-p) can never exceed 10% of the notional
	// while rate ≤ 0.10, so a larger value means a misread schedule.
	if notional := shares * price; notional > 0 && fee > notional*maxFeeRate+1e-9 {
		return 0, fmt.Errorf("手续费 %.8f 超过名义金额 %.8f 的 %.0f%%", fee, notional, maxFeeRate*100)
	}
	return fee, nil
}

// applyFee books the taker fee on top of the fill cost. Polymarket documents a
// market BUY's amount as the pre-fee notional with the taker fee charged on
// top in USDC (docs.polymarket.com/trading/fees), so the shares bought are left
// untouched and the fee is added to Cost. Price stays the execution price; the
// fee is carried separately so paper and live entry prices are comparable.
//
// An error here must reach the caller: a fee that cannot be computed used to be
// dropped, which booked real fills at zero fee and overstated PnL.
func applyFee(schedule clobclient.FeeSchedule, fill *engine.LiveFill) error {
	if fill == nil || fill.Shares <= 0 || fill.Price <= 0 || fill.Price >= 1 {
		return nil
	}
	fee, err := feeFromSchedule(schedule, fill.Shares, fill.Price)
	if err != nil {
		return fmt.Errorf("计算实盘手续费: %w", err)
	}
	fill.Fee = fee
	fill.Cost += fee
	return nil
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// normalizeBuilderCode validates the 32-byte builder code that attributes
// orders to a Polymarket builder profile. An empty code is allowed: the order
// is then signed with the zero hash and the trade is not attributed.
func normalizeBuilderCode(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	decoded, err := hexutil.Decode(value)
	if err != nil || len(decoded) != common.HashLength {
		return "", fmt.Errorf("开发者码必须是 0x 开头的 32 字节十六进制字符串")
	}
	return strings.ToLower(value), nil
}

func (e *Executor) Collateral(ctx context.Context) (engine.CashSnapshot, error) {
	e.mu.Lock()
	if time.Since(e.cashAt) < 15*time.Second && !e.cash.At.IsZero() {
		snapshot := e.cash
		e.mu.Unlock()
		return snapshot, nil
	}
	cached := e.cash
	e.mu.Unlock()

	client, err := e.ensure(ctx)
	if err != nil {
		return engine.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, At: cached.At, Stale: true}, err
	}
	balance, err := client.BalanceAllowance(ctx, "COLLATERAL", "")
	if err != nil {
		return engine.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, At: cached.At, Stale: true}, fmt.Errorf("读取实盘 USDC 余额: %w", err)
	}
	units, err := strconv.ParseFloat(strings.TrimSpace(balance.Balance), 64)
	if err != nil {
		return engine.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, At: cached.At, Stale: true}, fmt.Errorf("解析实盘 USDC 余额 %q: %w", balance.Balance, err)
	}
	allowanceUnits := 0.0
	if raw := strings.TrimSpace(balance.Allowance); raw != "" {
		allowanceUnits, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return engine.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, At: cached.At, Stale: true}, fmt.Errorf("解析实盘 USDC 授权额度 %q: %w", balance.Allowance, err)
		}
	}
	snapshot := engine.CashSnapshot{Amount: units / 1_000_000, Allowance: allowanceUnits / 1_000_000, At: time.Now().UTC()}
	e.mu.Lock()
	e.cash = snapshot
	e.cashAt = snapshot.At
	e.mu.Unlock()
	return snapshot, nil
}

func fillFromOrder(detail *clobclient.Order, limitPrice float64) (engine.LiveFill, bool) {
	if detail == nil {
		return engine.LiveFill{}, false
	}
	shares := parseAmount(detail.SizeMatched)
	price := parseAmount(detail.AvgPrice)
	if price <= 0 {
		price = parseAmount(detail.Price)
	}
	if price <= 0 {
		price = limitPrice
	}
	if shares <= 0 || price <= 0 {
		return engine.LiveFill{}, false
	}
	return engine.LiveFill{
		OrderID: detail.ID,
		Shares:  shares,
		Cost:    shares * price,
		Price:   price,
		Status:  detail.Status,
	}, true
}

func parseAmount(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return value
}
