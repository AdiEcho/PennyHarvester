// Package live is the real-money execution adapter. It only places orders
// through Polymarket's CLOB when a private key is explicitly configured.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/venue"
)

type Config struct {
	PrivateKey    string
	Funder        string
	SignatureType int
	CLOBURL       string
	// RelayerURL points at the gasless relayer. It is used for credential
	// checks only; orders always go through the CLOB.
	RelayerURL    string
	APIKey        string
	APISecret     string
	APIPassphrase string
	// BuilderCode attributes every signed order to a Polymarket builder
	// profile. It is public and may be empty (unattributed orders).
	BuilderCode string
	// PolygonRPC is a read-only Polygon JSON-RPC endpoint used by wallet
	// detection (eth_getCode / eth_call). It never signs anything.
	PolygonRPC string
	// Proxy is the outbound egress shared with the market data client. The
	// executor rebuilds its CLOB client when the system settings change.
	Proxy *netproxy.Manager
}

type Executor struct {
	// base is the immutable startup configuration from the process environment.
	// cfg is the effective configuration after web-managed overrides.
	base Config
	cfg  Config

	mu     sync.Mutex
	credMu sync.Mutex
	client *clobclient.Client
	// readClient is a credential-free client for public reads such as the
	// radar quote. It shares the effective CLOB URL and proxy but never signs.
	readClient  *clobclient.Client
	initErr     error
	credentials *clobclient.Credentials
	cash        engine.CashSnapshot
	cashAt      time.Time
	generation  uint64
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
	cfg.RelayerURL = strings.TrimRight(strings.TrimSpace(cfg.RelayerURL), "/")
	cfg.PolygonRPC = strings.TrimRight(strings.TrimSpace(cfg.PolygonRPC), "/")
	builderCode, builderErr := normalizeBuilderCode(cfg.BuilderCode)
	return &Executor{base: cfg, cfg: cfg, builderCode: builderCode, builderErr: builderErr}
}

func normalizeKeyPrefix(value string) string {
	if len(value) >= 2 && strings.EqualFold(value[:2], "0x") {
		return "0x" + value[2:]
	}
	return value
}

// Venue reports which exchange this executor trades on. It is what lets the
// venue registry route an order to the adapter that owns its identifiers;
// every order signed by this executor is a Polymarket CLOB order.
func (e *Executor) Venue() market.Venue { return market.VenuePolymarket }

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
	e.mu.Lock()
	defer e.mu.Unlock()
	effective, err := e.resolveLocked(settings, clobBaseURL)
	if err != nil {
		return err
	}
	if sameLiveConfig(e.cfg, effective) {
		return nil
	}
	e.cfg = effective
	e.client = nil
	e.readClient = nil
	e.initErr = nil
	e.credentials = nil
	e.cash = engine.CashSnapshot{}
	e.cashAt = time.Time{}
	e.generation++
	return nil
}

// resolveLocked merges web-managed overrides onto the startup configuration
// without touching the running executor. Callers must hold e.mu.
func (e *Executor) resolveLocked(settings engine.LiveSettings, clobBaseURL string) (Config, error) {
	settings = settings.Normalize()
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
		return Config{}, fmt.Errorf("POLYMARKET_API_KEY、POLYMARKET_API_SECRET、POLYMARKET_API_PASSPHRASE 必须同时设置")
	}
	if credentialCount == 3 {
		effective.APIKey = settings.APIKey
		effective.APISecret = settings.APISecret
		effective.APIPassphrase = settings.APIPassphrase
	}
	if err := validateEffectiveConfig(effective, e.builderErr); err != nil {
		return Config{}, err
	}
	return effective, nil
}

// Probe validates candidate credentials against the CLOB without placing an
// order or disturbing the running executor: it derives (or checks) the L2
// credentials and reads the wallet's collateral balance. The CLOB answers both
// calls only when the private key, signature type and funder are consistent,
// so a success here means the next order will not fail on authentication.
func (e *Executor) Probe(ctx context.Context, settings engine.LiveSettings, clobBaseURL string) (engine.LiveProbe, error) {
	if e == nil {
		return engine.LiveProbe{}, fmt.Errorf("未配置实盘执行器")
	}
	e.mu.Lock()
	effective, err := e.resolveLocked(settings, clobBaseURL)
	e.mu.Unlock()
	if err != nil {
		return engine.LiveProbe{}, err
	}
	probe := New(effective)
	start := time.Now()
	if _, err := probe.ensure(ctx); err != nil {
		return engine.LiveProbe{}, err
	}
	snapshot, err := probe.Collateral(ctx)
	if err != nil {
		return engine.LiveProbe{}, err
	}
	probe.mu.Lock()
	credentials := probe.credentials
	probe.mu.Unlock()
	source := "derived"
	if strings.TrimSpace(effective.APIKey) != "" {
		source = "configured"
	}
	apiKey := ""
	if credentials != nil {
		apiKey = maskAPIKey(credentials.APIKey)
	}
	return engine.LiveProbe{
		Signer:        probe.Signer(),
		Maker:         probe.Maker(),
		APIKey:        apiKey,
		Source:        source,
		Mode:          walletMode(effective.SignatureType),
		SignatureType: effective.SignatureType,
		Cash:          snapshot.Amount,
		Allowance:     snapshot.Allowance,
		LatencyMS:     float64(time.Since(start).Microseconds()) / 1000,
	}, nil
}

// Wallet modes split the two ways a Polymarket account can hold funds. The
// mode decides which addresses matter: an EOA is its own maker, while a proxy
// wallet is owned by the signer but holds the collateral.
const (
	WalletModeEOA   = "eoa"
	WalletModeProxy = "proxy"
)

func walletMode(signatureType int) string {
	if signatureType == 0 {
		return WalletModeEOA
	}
	return WalletModeProxy
}

// maskAPIKey keeps the leading bytes of the UUID so the operator can match the
// value against the CLOB dashboard without exposing the whole credential.
func maskAPIKey(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "…"
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
		PolygonRPCURL:    effectiveRPC(base.PolygonRPC),
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
	signer := ""
	if key != "" {
		if !strings.HasPrefix(strings.ToLower(key), "0x") {
			return fmt.Errorf("POLYMARKET_PRIVATE_KEY 必须以 0x 开头")
		}
		privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(key, "0x"))
		if err != nil {
			return fmt.Errorf("POLYMARKET_PRIVATE_KEY 无效: %w", err)
		}
		signer = crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
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
			return fmt.Errorf("EOA 签名类型不需要 POLYMARKET_FUNDER；请留空，或改用代理钱包模式（签名类型 1/2/3）")
		}
	} else if key != "" && funder == "" {
		return fmt.Errorf("代理钱包模式需要设置 POLYMARKET_FUNDER（Polymarket 账户里的代理 / Deposit 钱包地址）")
	}
	// The funder must be the smart wallet that holds the collateral, never the
	// signer itself; otherwise orders are signed for an address that has no
	// balance and every fill fails after the order is already on the wire.
	if signer != "" && funder != "" && strings.EqualFold(signer, funder) {
		return fmt.Errorf("POLYMARKET_FUNDER 不能等于私钥对应的签名地址 %s；请填写 Polymarket 账户里的代理 / Deposit 钱包地址", signer)
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
	client, err := newClobClient(e.cfg, e.credentials)
	if err != nil {
		e.initErr = fmt.Errorf("初始化 Polymarket CLOB 客户端: %w", err)
		return
	}
	e.client = client
	e.signer = client.Address()
}

// readClientLocked returns a credential-free CLOB client for public reads. It
// uses the same CLOB URL and outbound proxy as the trading client but strips
// every wallet field, so a radar quote works before (or without) live
// credentials. Callers must hold e.mu.
func (e *Executor) readClientLocked() (*clobclient.Client, error) {
	if e.readClient != nil {
		return e.readClient, nil
	}
	cfg := e.cfg
	cfg.PrivateKey = ""
	cfg.Funder = ""
	cfg.SignatureType = 0
	cfg.APIKey, cfg.APISecret, cfg.APIPassphrase = "", "", ""
	client, err := newClobClient(cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("初始化 Polymarket CLOB 只读客户端: %w", err)
	}
	e.readClient = client
	return client, nil
}

// newClobClient builds a CLOB client for cfg, sharing the outbound proxy with
// market data. credentials may be nil; the client then derives them on first
// use.
func newClobClient(cfg Config, credentials *clobclient.Credentials) (*clobclient.Client, error) {
	funder := strings.TrimSpace(cfg.Funder)
	makerAddress := ""
	if cfg.SignatureType != 0 {
		makerAddress = funder
	}
	sdkCfg := clobclient.Config{
		PrivateKey:    strings.TrimSpace(cfg.PrivateKey),
		MakerAddress:  makerAddress,
		SignatureType: clobclient.SignatureType(cfg.SignatureType),
		Host:          cfg.CLOBURL,
		Timeout:       20 * time.Second,
		QPS:           10,
	}
	credentialCount := 0
	for _, value := range []string{cfg.APIKey, cfg.APISecret, cfg.APIPassphrase} {
		if strings.TrimSpace(value) != "" {
			credentialCount++
		}
	}
	if credentialCount == 3 {
		sdkCfg.Credentials = &clobclient.Credentials{
			APIKey: cfg.APIKey, Secret: cfg.APISecret, Passphrase: cfg.APIPassphrase,
		}
	} else if credentials != nil {
		// Reuse credentials derived before the client was rebuilt for a new
		// proxy, instead of hitting the CLOB credential endpoint again.
		sdkCfg.Credentials = credentials
	}
	if cfg.Proxy != nil {
		if cfg.Proxy.Config().Mode == netproxy.ModeOff {
			// The SDK has no "ignore the environment" switch, so it gets a
			// prebuilt client that connects directly. Reads and orders then
			// share one pool, which only happens in this opt-out mode.
			sdkCfg.HTTPClient = cfg.Proxy.HTTPClient(20 * time.Second)
		} else {
			sdkCfg.ProxyURL = cfg.Proxy.ProxyURL()
		}
	}
	return clobclient.New(sdkCfg)
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
	e.readClient = nil
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
	plan, err := planMarketBuy(ctx, client, tokenID, amount)
	if err != nil {
		return engine.LiveFill{}, fmt.Errorf("构建实盘买单: %w", err)
	}
	if plan.available+1e-9 < amount {
		return engine.LiveFill{}, fmt.Errorf("构建实盘买单: 盘口深度不足，需要 %.4f USDC，可用 %.4f USDC", amount, plan.available)
	}
	order := clobclient.UserOrder{TokenID: tokenID, Side: clobclient.SideBuy, Price: plan.limitPrice, Shares: plan.shares, OrderType: clobclient.OrderTypeFAK}
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
		// success=false is an explicit rejection, not an unknown outcome. The
		// SDK returns it together with a response and OrderRejectedError; doing
		// a 404 lookup first would turn a definite rejection into a blocked
		// unknown. Timeouts and transport errors keep the lookup path.
		var rejected *clobclient.OrderRejectedError
		if errors.As(submitErr, &rejected) {
			message := strings.TrimSpace(rejected.Message)
			if message == "" {
				message = "Polymarket 拒绝了订单"
			}
			return engine.LiveFill{OrderID: signed.OrderID, Status: "rejected"}, fmt.Errorf("%s", message)
		}
		// A timeout can still mean the exchange accepted the order. Reconcile
		// the signed digest, but book only a complete CONFIRMED trade set; a
		// size_matched estimate is not a final fill.
		return e.lookupAmbiguousSubmit(ctx, signed.OrderID, conditionID,
			fmt.Errorf("提交实盘买单（订单 %s，结果可能未知）: %w", signed.OrderID, submitErr))
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

// Sell signs and submits a FOK market sell for the full requested share count.
// FOK is deliberate for stop-loss exits: a partial fill would leave a residual
// position that the local ledger would have to track as a separate lot. If the
// book cannot take every share, the order is killed and the next scan retries.
//
// The CLOB response reports makingAmount as shares and takingAmount as pUSD for
// a sell, the reverse of a buy. LiveFill.Cost stays the gross proceeds and the
// taker fee is reported separately in LiveFill.Fee, so the engine subtracts it
// exactly once.
func (e *Executor) Sell(ctx context.Context, tokenID, conditionID string, shares float64, onSigned func(orderID string) error) (engine.LiveFill, error) {
	if tokenID == "" || conditionID == "" || shares <= 0 || math.IsNaN(shares) || math.IsInf(shares, 0) {
		return engine.LiveFill{}, fmt.Errorf("%w: token、condition 与份额必须有效", engine.ErrInvalidOrder)
	}
	client, err := e.ensure(ctx)
	if err != nil {
		return engine.LiveFill{}, err
	}
	schedule, err := client.FeeSchedule(ctx, conditionID)
	if err != nil {
		return engine.LiveFill{}, fmt.Errorf("读取实盘手续费: %w", err)
	}
	// MarketSellOrder reads the SDK's cached tick (and CreateOrder reuses it).
	// Drop stale metadata first so the sell price is floored on today's tick.
	client.InvalidateMarketMetadata(tokenID)
	order, err := client.MarketSellOrder(ctx, tokenID, shares, clobclient.OrderTypeFOK)
	if err != nil {
		return engine.LiveFill{}, fmt.Errorf("构建实盘卖单: %w", err)
	}
	order.Builder = e.builderCode
	signed, err := client.CreateOrder(ctx, order)
	if err != nil {
		return engine.LiveFill{}, fmt.Errorf("签名实盘卖单: %w", err)
	}
	if onSigned != nil {
		if err := onSigned(signed.OrderID); err != nil {
			return engine.LiveFill{}, fmt.Errorf("持久化待确认实盘卖单: %w", err)
		}
	}
	response, submitErr := client.SubmitSignedOrder(ctx, signed, clobclient.OrderTypeFOK, false)
	if submitErr != nil {
		var rejected *clobclient.OrderRejectedError
		if errors.As(submitErr, &rejected) {
			message := strings.TrimSpace(rejected.Message)
			if message == "" {
				message = "Polymarket 拒绝了卖单"
			}
			return engine.LiveFill{OrderID: signed.OrderID, Status: "rejected"}, fmt.Errorf("%s", message)
		}
		return e.lookupAmbiguousSubmit(ctx, signed.OrderID, conditionID,
			fmt.Errorf("提交实盘卖单（订单 %s，结果可能未知）: %w", signed.OrderID, submitErr))
	}
	if response == nil {
		return engine.LiveFill{OrderID: signed.OrderID, Status: "unknown"}, fmt.Errorf("Polymarket 返回空订单响应")
	}
	if !response.Success {
		message := strings.TrimSpace(response.ErrorMsg)
		if message == "" {
			message = "Polymarket 拒绝了卖单"
		}
		if response.OrderID != "" {
			message = fmt.Sprintf("%s（订单 %s）", message, response.OrderID)
		}
		return engine.LiveFill{OrderID: response.OrderID, Status: "rejected"}, fmt.Errorf("%s", message)
	}
	fill := engine.LiveFill{
		OrderID: response.OrderID, Status: response.Status,
		Shares: parseAmount(response.MakingAmount), Cost: parseAmount(response.TakingAmount),
	}
	if fill.Shares <= 0 || fill.Cost <= 0 {
		originalStatus := fill.Status
		if fill.OrderID == "" {
			fill.OrderID = signed.OrderID
		}
		fill.Status = "unknown"
		return fill, fmt.Errorf("实盘卖单未产生可确认成交（原状态 %s）", originalStatus)
	}
	fill.Price = fill.Cost / fill.Shares
	if err := reportSellFee(schedule, &fill); err != nil {
		return engine.LiveFill{OrderID: fill.OrderID, Status: "unknown"}, err
	}
	return fill, nil
}

// reportSellFee reports the taker fee on a sell without folding it into Cost.
// Fill.Cost is the gross proceeds; the engine subtracts Fee exactly once, so
// pre-deducting it here would charge the seller twice.
func reportSellFee(schedule clobclient.FeeSchedule, fill *engine.LiveFill) error {
	if fill == nil || fill.Shares <= 0 || fill.Price <= 0 || fill.Price >= 1 {
		return nil
	}
	fee, err := feeFromSchedule(schedule, fill.Shares, fill.Price)
	if err != nil {
		return fmt.Errorf("计算实盘卖出手续费: %w", err)
	}
	if fee >= fill.Cost {
		return fmt.Errorf("卖出手续费 %.8f 不小于成交所得 %.8f", fee, fill.Cost)
	}
	fill.Fee = fee
	return nil
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

// polymarketMinBuyNotional is Polymarket's floor for a marketable BUY, in
// USDC. It mirrors venue.PolymarketCapabilities().MinNotional so the executor
// and the venue fact sheet cannot drift apart.
var polymarketMinBuyNotional = venue.PolymarketCapabilities().MinNotional

// askLevel is one parsed ask level on the book.
type askLevel struct{ price, size float64 }

// buyPlan is the FAK order a dollar amount becomes against the live ask book:
// the worst touched level ceiled to the tick, and the shares that budget buys.
type buyPlan struct {
	levels     []askLevel // asks sorted by price, cheapest first
	available  float64    // USDC the whole ask side can absorb
	limitPrice float64    // worst touched ask, ceiled to the market tick
	shares     float64
}

// planMarketBuy sizes a FAK marketable BUY from the real ask book.
//
// It mirrors clobclient.MarketBuyOrder (worst touched level, tick-ceiled limit
// price, shares floored to four decimals) with one deliberate difference: it
// enforces Polymarket's dollar floor rather than the book's min_order_size.
// min_order_size is the five-share floor for resting limit orders; a marketable
// BUY is sized in USDC and the exchange only requires one dollar, which is why
// the web UI fills a $2 buy at 0.98 (2.04 shares) while the SDK refuses it.
func alignTick(price, tick float64) float64 {
	return math.Round(price/tick) * tick
}

// ceilTick rounds price up to the next tick without letting float noise
// (0.07/0.01 == 7.000000000000001) add a tick that the price already sits on.
func ceilTick(price, tick float64) float64 {
	return alignTick(math.Ceil(price/tick-1e-9)*tick, tick)
}

func planMarketBuy(ctx context.Context, client *clobclient.Client, tokenID string, amount float64) (buyPlan, error) {
	book, err := client.OrderBook(ctx, tokenID)
	if err != nil {
		return buyPlan{}, fmt.Errorf("读取实盘订单簿: %w", err)
	}
	levels := make([]askLevel, 0, len(book.Asks))
	for _, ask := range book.Asks {
		price, priceErr := strconv.ParseFloat(ask.Price, 64)
		size, sizeErr := strconv.ParseFloat(ask.Size, 64)
		if priceErr != nil || sizeErr != nil || price <= 0 || price >= 1 || size <= 0 {
			continue
		}
		levels = append(levels, askLevel{price: price, size: size})
	}
	if len(levels) == 0 {
		return buyPlan{}, fmt.Errorf("订单簿没有可用卖盘")
	}
	sort.Slice(levels, func(i, j int) bool { return levels[i].price < levels[j].price })
	available := 0.0
	for _, level := range levels {
		available += level.price * level.size
	}

	budget := amount
	if available < budget {
		budget = available
	}
	// The limit price is the worst level the budget touches, tick-ceiled. Risk
	// checks compare it against max_price, so it stays separate from the
	// average fill price the quote reports.
	remaining, limitPrice := budget, levels[0].price
	for _, level := range levels {
		limitPrice = level.price
		remaining -= level.price * level.size
		if remaining <= 0 {
			break
		}
	}
	bookTick := 0.0
	if parsed, tickErr := strconv.ParseFloat(strings.TrimSpace(book.TickSize), 64); tickErr == nil && parsed > 0 {
		bookTick = parsed
	}
	// Signing uses the SDK's cached tick, not the book snapshot. Refresh that
	// cache when it disagrees so the plan and the signature use one tick.
	tick, tickErr := client.TickSize(ctx, tokenID)
	if tickErr != nil || tick <= 0 {
		if bookTick > 0 {
			tick = bookTick
		} else {
			return buyPlan{}, fmt.Errorf("读取最小价位: %w", tickErr)
		}
	} else if bookTick > 0 && math.Abs(bookTick-tick) > 1e-9 {
		client.InvalidateMarketMetadata(tokenID)
		if refreshed, refreshErr := client.TickSize(ctx, tokenID); refreshErr == nil && refreshed > 0 {
			tick = refreshed
		}
	}
	limitPrice = ceilTick(limitPrice, tick)
	if limitPrice <= 0 || limitPrice >= 1 {
		return buyPlan{}, fmt.Errorf("盘口价格 %.6f 无法成交", limitPrice)
	}
	// The exchange keeps four decimals for a FAK buy's share amount and floors
	// the signed USDC amount to cents. A price of 0.98 with 2.0408 shares signs
	// as $1.99 / 2.0408 = 0.9751, below the limit, and the exchange rejects it.
	// Raise the signed limit by whole ticks (and resize the shares to stay in
	// budget) until the signed maker/taker ratio still crosses the ask.
	const shareScale = 10_000
	bestAsk := levels[0].price
	shares, signedNotional := 0.0, 0.0
	for {
		shares = math.Floor((budget/limitPrice)*shareScale) / shareScale
		if shares <= 0 {
			return buyPlan{}, fmt.Errorf("盘口深度不足，无法买入")
		}
		signedNotional = math.Floor(limitPrice*shares*100) / 100
		if signedNotional/shares+1e-9 >= bestAsk {
			break
		}
		limitPrice = alignTick(limitPrice+tick, tick)
		if limitPrice >= 1 {
			break
		}
	}
	if minimum := polymarketMinBuyNotional; minimum > 0 && signedNotional+1e-9 < minimum {
		return buyPlan{}, fmt.Errorf("下单金额 %.2f USDC 低于市场最小下单额 %.2f USDC（卖盘深度 %.4f USDC）", signedNotional, minimum, available)
	}
	if signedNotional/shares+1e-9 < bestAsk {
		return buyPlan{}, fmt.Errorf("盘口价格 %.6f 按最小价位签名后无法成交", bestAsk)
	}
	return buyPlan{levels: levels, available: available, limitPrice: limitPrice, shares: shares}, nil
}

// Quote estimates the result of a live FAK market buy using the real CLOB ask
// book and the market's real taker fee schedule. It mirrors MarketBuyOrder's
// share sizing (worst touched level, tick-ceiled), see planMarketBuy, but
// prices every level at its own ask, so Notional is the expected making amount
// rather than the limit price applied to the whole size. It never signs or
// submits anything. When the book cannot fill the full amount it quotes the
// available depth and sets FullyFilled=false.
func (e *Executor) Quote(ctx context.Context, tokenID, conditionID string, amount float64) (engine.LiveQuote, error) {
	if e == nil {
		return engine.LiveQuote{}, fmt.Errorf("未配置实盘执行器")
	}
	if strings.TrimSpace(tokenID) == "" || strings.TrimSpace(conditionID) == "" {
		return engine.LiveQuote{}, fmt.Errorf("token 与 condition 必须有效")
	}
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return engine.LiveQuote{}, fmt.Errorf("买入金额必须为有限正数")
	}
	e.mu.Lock()
	client, err := e.readClientLocked()
	e.mu.Unlock()
	if err != nil {
		return engine.LiveQuote{}, err
	}
	plan, err := planMarketBuy(ctx, client, tokenID, amount)
	if err != nil {
		return engine.LiveQuote{}, err
	}
	schedule, err := client.FeeSchedule(ctx, conditionID)
	if err != nil {
		return engine.LiveQuote{}, fmt.Errorf("读取实盘手续费: %w", err)
	}

	// Walk the book again: a FAK order pays each maker its own ask, not the
	// limit price for every share.
	notional, unfilled := 0.0, plan.shares
	filled := make([]fillLevel, 0, len(plan.levels))
	for _, level := range plan.levels {
		take := math.Min(unfilled, level.size)
		if take <= 0 {
			break
		}
		notional += take * level.price
		filled = append(filled, fillLevel{shares: take, price: level.price})
		unfilled -= take
	}
	if unfilled > 1e-6 {
		return engine.LiveQuote{}, fmt.Errorf("盘口深度不足，无法覆盖 %.4f 份", plan.shares)
	}
	fillPrice := math.Round((notional/plan.shares)*1e6) / 1e6
	fee, err := feeFromLevels(schedule, filled)
	if err != nil {
		return engine.LiveQuote{}, err
	}
	rate, _ := feeRate(schedule)
	return engine.LiveQuote{
		BestAsk: plan.levels[0].price, LimitPrice: plan.limitPrice, FillPrice: fillPrice,
		Shares: plan.shares, Notional: notional, Fee: fee, FeeRate: rate,
		AvailableNotional: plan.available, FullyFilled: plan.available+1e-9 >= amount,
	}, nil
}

// lookupAmbiguousSubmit reconciles an order whose submit response did not
// prove a fill. It returns a bookable fill only when LookupOrder can prove a
// complete CONFIRMED trade set; otherwise the order stays unknown with an error
// so the caller keeps it for reconciliation instead of booking a size_matched ×
// limit-price estimate.
func (e *Executor) lookupAmbiguousSubmit(ctx context.Context, orderID, conditionID string, cause error) (engine.LiveFill, error) {
	fill, found, lookupErr := e.LookupOrder(ctx, orderID, conditionID)
	if lookupErr == nil && found && fill.Shares > 0 && fill.Cost > 0 {
		return fill, nil
	}
	reason := fmt.Sprintf("订单 %s 结果待确认: %v", orderID, cause)
	if lookupErr != nil {
		reason = fmt.Sprintf("%s（对账: %v）", reason, lookupErr)
	}
	return engine.LiveFill{OrderID: orderID, Status: "unknown"}, errors.New(reason)
}

// LookupOrder reconciles an order by its signed digest / exchange order ID.
//
// The exchange reports the order's matched quantity and limit price, not the
// average execution price. A positive size_matched is therefore not a fill:
// a match can still be retried, mined or failed. The order is booked only when
// every trade row is CONFIRMED, the cursor chain has been exhausted, and the
// confirmed shares equal size_matched. Anything else returns an unknown fill
// with an error so the engine leaves the order unresolved instead of booking a
// limit-price estimate the exchange has not settled.
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
	expected := parseAmount(detail.SizeMatched)
	trades, tradeErr := e.tradesForOrder(ctx, orderID, conditionID, expected)
	if tradeErr != nil {
		// The limit-price estimate is deliberately not returned: it is not a
		// confirmed fill, and a failed or still-processing match must never be
		// booked as shares. The error keeps the order unresolved upstream.
		return engine.LiveFill{OrderID: orderID, Status: "unknown"}, true, fmt.Errorf("对账订单 %s 成交: %w", orderID, tradeErr)
	}
	if trades.ConfirmedShares <= 0 {
		// A verified empty fill set: keep the order's own status so the engine
		// can distinguish an open order from a terminal rejection.
		return engine.LiveFill{OrderID: orderID, Status: detail.Status}, true, nil
	}
	fill := engine.LiveFill{
		OrderID: orderID,
		Shares:  trades.ConfirmedShares,
		Cost:    trades.ConfirmedNotional,
		Price:   trades.ConfirmedNotional / trades.ConfirmedShares,
		Status:  "CONFIRMED",
	}
	if conditionID != "" {
		schedule, scheduleErr := client.FeeSchedule(ctx, conditionID)
		if scheduleErr != nil {
			return engine.LiveFill{OrderID: orderID, Status: "unknown"}, true, fmt.Errorf("对账时读取实盘手续费: %w", scheduleErr)
		}
		// Polymarket's fee is nonlinear in price, so compute it per confirmed
		// trade instead of applying the schedule to the VWAP. Report it without
		// folding it into Cost; the engine adds or subtracts it once.
		fee, feeErr := feeFromLevels(schedule, trades.ConfirmedLevels)
		if feeErr != nil {
			return engine.LiveFill{OrderID: orderID, Status: "unknown"}, true, feeErr
		}
		fill.Fee = fee
	}
	return fill, true, nil
}

// maxFeeRate bounds the taker rate the exchange is allowed to report. The
// documented categories top out at 0.07; anything higher means the schedule is
// being read wrong, and silently booking a smaller fee would overstate PnL.
//
// The schedule must come from /clob-markets/{condition_id} (the `fd` object),
// never from the legacy /fee-rate route: that route still answers with the old
// base fee in basis points (1000 = 10% on a fee-free market), which is not the
// rate the exchange charges at match time.
const maxFeeRate = 0.10

// feeRate validates and parses a schedule's rate. An empty rate means the
// market charges no fee.
func feeRate(schedule clobclient.FeeSchedule) (float64, error) {
	raw := strings.TrimSpace(schedule.Rate.String())
	if raw == "" {
		return 0, nil
	}
	rate, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("解析手续费率 %q: %w", raw, err)
	}
	if rate < 0 || rate > maxFeeRate {
		return 0, fmt.Errorf("手续费率 %q 超出 0–%.2f 范围", raw, maxFeeRate)
	}
	return rate, nil
}

func feeFromSchedule(schedule clobclient.FeeSchedule, shares, price float64) (float64, error) {
	if _, err := feeRate(schedule); err != nil {
		return 0, err
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

// fillLevel is one ask level consumed by a FAK order.
type fillLevel struct {
	shares float64
	price  float64
}

// feeFromLevels prices each swept ask as its own fill, which is how the
// exchange charges a FAK order: every maker match pays the schedule's fee at
// that match's price.
func feeFromLevels(schedule clobclient.FeeSchedule, levels []fillLevel) (float64, error) {
	if _, err := feeRate(schedule); err != nil {
		return 0, err
	}
	total, notional := 0.0, 0.0
	for _, level := range levels {
		raw, err := schedule.Fee(formatFloat(level.shares), formatFloat(level.price), true)
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
		total += fee
		notional += level.shares * level.price
	}
	if notional > 0 && total > notional*maxFeeRate+1e-9 {
		return 0, fmt.Errorf("手续费 %.8f 超过名义金额 %.8f 的 %.0f%%", total, notional, maxFeeRate*100)
	}
	return total, nil
}

// applyFee books the taker fee on top of the fill cost. Polymarket documents a
// market BUY's amount as the pre-fee notional with the taker fee charged on
// top in pUSD (docs.polymarket.com/trading/fees), so the shares bought are left
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
	// Report the fee without folding it into Cost: venue.Fill.Cost is the gross
	// notional, and the engine adds or subtracts the fee exactly once when it
	// books the position or the proceeds.
	fill.Fee = fee
	return nil
}

// guardedHTTPClient never follows a redirect on an authenticated request.
// Go's default redirect policy protects Authorization/Cookie but not the
// POLY_* and RELAYER_* headers, so a configured or compromised endpoint could
// otherwise forward exchange credentials to another host.
func guardedHTTPClient(timeout time.Duration, proxy *netproxy.Manager) *http.Client {
	client := &http.Client{Timeout: timeout}
	if proxy != nil {
		client = proxy.HTTPClient(timeout)
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
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
	generation := e.generation
	e.mu.Unlock()

	client, err := e.ensure(ctx)
	if err != nil {
		return engine.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, Allowances: cached.Allowances, At: cached.At, Stale: true}, err
	}
	balanceUnits, allowances, err := e.collateralBalance(ctx, client)
	if err != nil {
		return engine.CashSnapshot{Amount: cached.Amount, Allowance: cached.Allowance, Allowances: cached.Allowances, At: cached.At, Stale: true}, fmt.Errorf("读取实盘 pUSD 余额: %w", err)
	}
	// Keep the largest approval for display and legacy callers; the engine
	// guards each order against the exchange that will actually settle it.
	allowanceUnits := 0.0
	for _, value := range allowances {
		if value > allowanceUnits {
			allowanceUnits = value
		}
	}
	snapshot := engine.CashSnapshot{Amount: balanceUnits, Allowance: allowanceUnits, Allowances: allowances, At: time.Now().UTC()}
	e.mu.Lock()
	// A balance read started before a credential/host switch belongs to the
	// old wallet. Do not let its late response refill the new configuration's
	// cache as fresh data.
	if e.generation == generation {
		e.cash = snapshot
		e.cashAt = snapshot.At
	}
	e.mu.Unlock()
	return snapshot, nil
}

// collateralBalance reads the wallet's pUSD (COLLATERAL) balance and the
// approval it granted to the settling exchange.
//
// GET /balance-allowance answers with `allowances`, a map of spender address to
// amount, since a wallet holds one approval per exchange. The legacy scalar
// `allowance` field is no longer part of the response, and the CLOB client this
// build uses still decodes only that scalar, so reading it alone yields zero
// and the pre-trade allowance guard disappears without an error. The map is
// therefore the source of truth, with the scalar kept for older deployments and
// a fallback to the client so a rejected direct call never breaks the balance
// read.
func (e *Executor) collateralBalance(ctx context.Context, client *clobclient.Client) (float64, map[string]float64, error) {
	var response struct {
		Balance    json.Number            `json:"balance"`
		Allowance  json.Number            `json:"allowance"`
		Allowances map[string]json.Number `json:"allowances"`
	}
	query := url.Values{
		"asset_type":     {"COLLATERAL"},
		"signature_type": {strconv.Itoa(e.signatureType())},
	}
	if err := e.l2Get(ctx, "/balance-allowance", query, &response); err != nil {
		balance, allowance, sdkErr := e.collateralFromSDK(ctx, client)
		if sdkErr != nil {
			return 0, nil, err
		}
		return balance, map[string]float64{venue.PolymarketExchange: allowance}, nil
	}
	if strings.TrimSpace(response.Balance.String()) == "" {
		// A response without a balance must not be booked as zero: zero cash
		// would silently block every strategy order and misreport the wallet.
		balance, allowance, sdkErr := e.collateralFromSDK(ctx, client)
		if sdkErr != nil {
			return 0, nil, fmt.Errorf("CLOB 未返回 pUSD 余额")
		}
		return balance, map[string]float64{venue.PolymarketExchange: allowance}, nil
	}
	balance, err := collateralUnits(response.Balance)
	if err != nil {
		return 0, nil, fmt.Errorf("解析 pUSD 余额: %w", err)
	}
	allowances := make(map[string]float64, len(response.Allowances))
	for spender, raw := range response.Allowances {
		value, err := collateralUnits(raw)
		if err != nil {
			return 0, nil, fmt.Errorf("解析 pUSD 授权额度（%s）: %w", spender, err)
		}
		allowances[strings.ToLower(spender)] = value
	}
	if len(allowances) == 0 {
		// Older deployments answered with a single scalar instead of the map.
		if scalar, scalarErr := collateralUnits(response.Allowance); scalarErr == nil {
			allowances[venue.PolymarketExchange] = scalar
		}
	}
	return balance, allowances, nil
}

// collateralFromSDK is the fallback read through the CLOB client. It can only
// see the legacy scalar allowance, so it is used when the direct call fails.
func (e *Executor) collateralFromSDK(ctx context.Context, client *clobclient.Client) (float64, float64, error) {
	balance, err := client.BalanceAllowance(ctx, "COLLATERAL", "")
	if err != nil {
		return 0, 0, err
	}
	units, err := strconv.ParseFloat(strings.TrimSpace(balance.Balance), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("解析实盘 pUSD 余额 %q: %w", balance.Balance, err)
	}
	allowanceUnits := 0.0
	if raw := strings.TrimSpace(balance.Allowance); raw != "" {
		allowanceUnits, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("解析实盘 pUSD 授权额度 %q: %w", balance.Allowance, err)
		}
	}
	return units / 1_000_000, allowanceUnits / 1_000_000, nil
}

// signatureType reports the wallet mode the L2 read must use so the CLOB
// answers for the maker (funder) rather than the bare signer.
func (e *Executor) signatureType() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.SignatureType
}

func parseAmount(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return value
}
