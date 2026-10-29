// Package live is the real-money execution adapter. It only places orders
// through Polymarket's CLOB when a private key is explicitly configured.
package live

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"

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
	if e.cfg.SignatureType != 0 && strings.TrimSpace(e.cfg.Funder) == "" {
		e.initErr = fmt.Errorf("非 EOA 签名类型需要设置 POLYMARKET_FUNDER（代理钱包地址）")
		return
	}
	cfg := clobclient.Config{
		PrivateKey:    strings.TrimSpace(e.cfg.PrivateKey),
		MakerAddress:  strings.TrimSpace(e.cfg.Funder),
		SignatureType: clobclient.SignatureType(e.cfg.SignatureType),
		Host:          e.cfg.CLOBURL,
		Timeout:       20 * time.Second,
		QPS:           10,
	}
	if e.cfg.APIKey != "" && e.cfg.APISecret != "" && e.cfg.APIPassphrase != "" {
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

func (e *Executor) Buy(ctx context.Context, tokenID string, amount, maxPrice float64) (engine.LiveFill, error) {
	if tokenID == "" || amount <= 0 {
		return engine.LiveFill{}, fmt.Errorf("%w: token 与金额必须有效", engine.ErrInvalidOrder)
	}
	client, err := e.ensure(ctx)
	if err != nil {
		return engine.LiveFill{}, err
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
	response, submitErr := client.SubmitSignedOrder(ctx, signed, clobclient.OrderTypeFAK, false)
	if submitErr != nil {
		// A timeout can still mean the exchange accepted the order. Look the
		// signed digest up before reporting a rejection so a real fill is not
		// silently lost from the local ledger.
		if detail, lookupErr := client.Order(ctx, signed.OrderID); lookupErr == nil {
			if fill, ok := fillFromOrder(detail, order.Price); ok {
				return fill, nil
			}
		}
		return engine.LiveFill{}, fmt.Errorf("提交实盘买单（订单 %s，结果可能未知）: %w", signed.OrderID, submitErr)
	}
	if response == nil {
		return engine.LiveFill{}, fmt.Errorf("Polymarket 返回空订单响应")
	}
	if !response.Success {
		message := strings.TrimSpace(response.ErrorMsg)
		if message == "" {
			message = "Polymarket 拒绝了订单"
		}
		if response.OrderID != "" {
			message = fmt.Sprintf("%s（订单 %s）", message, response.OrderID)
		}
		return engine.LiveFill{}, fmt.Errorf("%s", message)
	}
	fill := engine.LiveFill{OrderID: response.OrderID, Status: response.Status}
	fill.Cost = parseAmount(response.MakingAmount)
	fill.Shares = parseAmount(response.TakingAmount)
	if fill.Shares <= 0 || fill.Cost <= 0 {
		if response.OrderID != "" {
			if detail, lookupErr := client.Order(ctx, response.OrderID); lookupErr == nil {
				if lookedUp, ok := fillFromOrder(detail, order.Price); ok {
					lookedUp.OrderID = response.OrderID
					return lookedUp, nil
				}
			}
		}
	}
	if fill.Shares <= 0 || fill.Cost <= 0 {
		return fill, fmt.Errorf("实盘订单未成交（状态 %s）", fill.Status)
	}
	fill.Price = fill.Cost / fill.Shares
	expected := fill.Shares * order.Price
	if fill.Cost > expected+1e-9 {
		fill.Fee = fill.Cost - expected
	}
	return fill, nil
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
		return engine.CashSnapshot{Amount: cached.Amount, At: cached.At, Stale: true}, err
	}
	balance, err := client.BalanceAllowance(ctx, "COLLATERAL", "")
	if err != nil {
		return engine.CashSnapshot{Amount: cached.Amount, At: cached.At, Stale: true}, fmt.Errorf("读取实盘 USDC 余额: %w", err)
	}
	units, err := strconv.ParseFloat(strings.TrimSpace(balance.Balance), 64)
	if err != nil {
		return engine.CashSnapshot{Amount: cached.Amount, At: cached.At, Stale: true}, fmt.Errorf("解析实盘 USDC 余额 %q: %w", balance.Balance, err)
	}
	snapshot := engine.CashSnapshot{Amount: units / 1_000_000, At: time.Now().UTC()}
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
