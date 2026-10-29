// Package live is the real-money execution adapter. It only places orders
// through Polymarket's CLOB when a private key is explicitly configured.
package live

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/ethereum/go-ethereum/common"

	"pennyharvester/backend/internal/engine"
)

type Config struct {
	PrivateKey    string
	Funder        string
	SignatureType int
	CLOBURL       string
	APIKey        string
	APISecret     string
	APIPassphrase string
}

type Executor struct {
	cfg Config

	mu      sync.Mutex
	credMu  sync.Mutex
	client  *clobclient.Client
	initErr error
	cash    engine.CashSnapshot
	cashAt  time.Time
	signer  string
}

func New(cfg Config) *Executor {
	return &Executor{cfg: cfg}
}

func (e *Executor) Configured() bool {
	return e != nil && strings.TrimSpace(e.cfg.PrivateKey) != ""
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
	if !e.Configured() {
		return "未配置 POLYMARKET_PRIVATE_KEY"
	}
	e.ensureClientLocked()
	if e.initErr != nil {
		return e.initErr.Error()
	}
	return ""
}

func (e *Executor) ensureClientLocked() {
	if e.client != nil || e.initErr != nil {
		return
	}
	if !e.Configured() {
		e.initErr = fmt.Errorf("未配置 POLYMARKET_PRIVATE_KEY")
		return
	}
	if e.cfg.SignatureType < 0 || e.cfg.SignatureType > 3 {
		e.initErr = fmt.Errorf("POLYMARKET_SIGNATURE_TYPE 仅支持 0、1、2、3")
		return
	}
	funder := strings.TrimSpace(e.cfg.Funder)
	if e.cfg.SignatureType == 0 {
		if funder != "" {
			e.initErr = fmt.Errorf("EOA 签名类型不需要 POLYMARKET_FUNDER；请留空，或改用 1/2/3")
			return
		}
	} else {
		if funder == "" {
			e.initErr = fmt.Errorf("非 EOA 签名类型需要设置 POLYMARKET_FUNDER（代理钱包地址）")
			return
		}
		if !common.IsHexAddress(funder) {
			e.initErr = fmt.Errorf("POLYMARKET_FUNDER 不是有效地址")
			return
		}
	}
	credentialCount := 0
	for _, value := range []string{e.cfg.APIKey, e.cfg.APISecret, e.cfg.APIPassphrase} {
		if strings.TrimSpace(value) != "" {
			credentialCount++
		}
	}
	if credentialCount != 0 && credentialCount != 3 {
		e.initErr = fmt.Errorf("POLYMARKET_API_KEY、POLYMARKET_API_SECRET、POLYMARKET_API_PASSPHRASE 必须同时设置")
		return
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
	}
	client, err := clobclient.New(cfg)
	if err != nil {
		e.initErr = fmt.Errorf("初始化 Polymarket CLOB 客户端: %w", err)
		return
	}
	e.client = client
	e.signer = client.Address()
}

func (e *Executor) ensure(ctx context.Context) (*clobclient.Client, error) {
	e.mu.Lock()
	e.ensureClientLocked()
	client := e.client
	initErr := e.initErr
	e.mu.Unlock()
	if initErr != nil {
		return nil, initErr
	}
	// Credential derivation is idempotent but does network I/O. Serialize it
	// separately so a transient failure can be retried on the next order.
	e.credMu.Lock()
	defer e.credMu.Unlock()
	if _, err := client.EnsureCredentials(ctx); err != nil {
		return nil, fmt.Errorf("派生 Polymarket CLOB API 凭据: %w", err)
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
				applyFee(schedule, &fill)
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
					applyFee(schedule, &lookedUp)
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
	applyFee(schedule, &fill)
	return fill, nil
}

// EstimateBuyFee returns the taker fee the exchange would charge for the
// requested notional at the reference price. It is used for pre-trade cash and
// exposure checks; the actual fee is recomputed from the fill.
func (e *Executor) EstimateBuyFee(ctx context.Context, conditionID string, amount, price float64) (float64, error) {
	if conditionID == "" || amount <= 0 || price <= 0 || price >= 1 {
		return 0, nil
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
		if schedule, scheduleErr := client.FeeSchedule(ctx, conditionID); scheduleErr == nil {
			applyFee(schedule, &fill)
		}
	}
	return fill, true, nil
}

func feeFromSchedule(schedule clobclient.FeeSchedule, shares, price float64) (float64, error) {
	raw, err := schedule.Fee(formatFloat(shares), formatFloat(price), true)
	if err != nil {
		return 0, err
	}
	fee, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("解析手续费 %q: %w", raw, err)
	}
	return fee, nil
}

func applyFee(schedule clobclient.FeeSchedule, fill *engine.LiveFill) {
	if fill == nil || fill.Shares <= 0 || fill.Price <= 0 || fill.Price >= 1 {
		return
	}
	fee, err := feeFromSchedule(schedule, fill.Shares, fill.Price)
	if err != nil {
		return
	}
	fill.Fee = fee
	fill.Cost += fee
	fill.Price = fill.Cost / fill.Shares
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
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
