package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/store"
	"pennyharvester/backend/internal/venue"
)

type chainFailure struct {
	hash     string
	reverted bool
}

func (e *chainFailure) Error() string {
	return "链上交易结果未知 " + e.hash
}

func (e *chainFailure) TransactionHash() string { return e.hash }

func (e *chainFailure) TransactionReverted() bool { return e.reverted }

type settledChainFixture struct {
	instance   *engine.Engine
	repository *store.SQLite
	live       *redeemLive
	accountID  int64
	position   engine.Position
}

func newSettledChainFixture(t *testing.T) *settledChainFixture {
	t.Helper()
	ctx := context.Background()
	pfMarket := binary("pf", 0.40, 20000, 2)
	pfMarket.FeeRateBPS = 200
	pfMarket.Venue = market.VenuePredictFun
	pfMarket.Currency = market.CurrencyUSDT
	provider := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": pfMarket}}
	live := &redeemLive{fakeLive: &fakeLive{
		balance: 1000, allowance: 1000,
		fill: engine.LiveFill{Shares: 100, Cost: 40, Price: 0.40, Status: "matched"},
	}}
	caps := testCapabilities(market.VenuePredictFun)
	caps.SupportsSettlement = true
	caps.SupportsRedeem = true
	registry := venue.NewRegistry()
	addVenueCaps(t, registry, caps, provider, live)
	instance, repository := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
	account, err := instance.CreateAccount(ctx, "PF live", engine.AccountModeLive, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PlaceOrder(ctx, account.ID, engine.OrderRequest{
		MarketID: "pf", Outcome: "Yes", TokenID: "pf-yes", Amount: 40,
	}); err != nil {
		t.Fatal(err)
	}
	pfMarket.Closed = true
	pfMarket.Active = false
	pfMarket.ResolutionStatus = "resolved"
	pfMarket.Outcomes[0].Price = 1
	pfMarket.Outcomes[1].Price = 0
	provider.markets["pf"] = pfMarket
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	won, err := instance.Positions(ctx, account.ID, "won", "")
	if err != nil || won.Total != 1 {
		t.Fatalf("expected one settled position: %+v err=%v", won.Items, err)
	}
	return &settledChainFixture{instance: instance, repository: repository, live: live, accountID: account.ID, position: won.Items[0]}
}

func (f *settledChainFixture) pendingRedeemOrder(t *testing.T) engine.Order {
	t.Helper()
	order, err := f.repository.CreateOrder(context.Background(), engine.Order{
		AccountID: f.accountID, Venue: market.VenuePredictFun, Currency: market.CurrencyUSDT,
		PositionID: &f.position.ID, MarketID: f.position.MarketID,
		Question: f.position.Question, Outcome: f.position.Outcome, TokenID: f.position.TokenID,
		Kind: f.position.Kind, Type: "settle", Action: "redeem",
		Price: f.position.CurrentPrice, Shares: f.position.Shares, Amount: f.position.Shares,
		Status: "pending", Reason: "链上赎回已提交，等待交易回执", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return order
}

func (f *settledChainFixture) redeemOrder(t *testing.T) engine.Order {
	t.Helper()
	orders, err := f.instance.Orders(context.Background(), f.accountID, "all", 50, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range orders.Items {
		if order.Action == "redeem" {
			return order
		}
	}
	t.Fatal("redeem order not found")
	return engine.Order{}
}

func TestRedeemUnknownPersistsSignedIdentityAndBlocksDuplicate(t *testing.T) {
	ctx := context.Background()
	fixture := newSettledChainFixture(t)
	hash := "0x" + strings.Repeat("1", 64)
	fixture.live.hash = hash
	fixture.live.err = &chainFailure{hash: hash}

	result, err := fixture.instance.RedeemPosition(ctx, fixture.accountID, fixture.position.ID)
	if err == nil {
		t.Fatal("an unknown receipt must be reported as an error")
	}
	if result.TxHash != hash {
		t.Fatalf("result lost the signed hash: %q", result.TxHash)
	}
	op, ok, err := fixture.repository.ActiveChainOperation(ctx, fixture.accountID, market.VenuePredictFun, "redeem", &fixture.position.ID)
	if err != nil || !ok {
		t.Fatalf("active operation missing: ok=%v err=%v", ok, err)
	}
	if op.Status != "unknown" || op.TxHash != hash || op.FromAddress == "" || op.Nonce == nil || op.ChainID != 97 {
		t.Fatalf("unknown operation lost its transaction identity: %+v", op)
	}
	order := fixture.redeemOrder(t)
	if order.Status != "unknown" || !strings.Contains(order.Reason, hash) {
		t.Fatalf("unknown redemption order = %+v", order)
	}
	if _, err := fixture.instance.RedeemPosition(ctx, fixture.accountID, fixture.position.ID); !errors.Is(err, engine.ErrChainOperationActive) {
		t.Fatalf("second redemption must be refused while the first is unresolved, got %v", err)
	}
	if len(fixture.live.redemptions) != 1 {
		t.Fatalf("duplicate guard reached the chain: %d attempts", len(fixture.live.redemptions))
	}
}

func TestRedeemPersistsAdapterHashWithoutCallback(t *testing.T) {
	ctx := context.Background()
	fixture := newSettledChainFixture(t)
	hash := "0x" + strings.Repeat("7", 64)
	fixture.live.hash = hash
	fixture.live.err = &chainFailure{hash: hash}
	fixture.live.skipSigned = true

	result, err := fixture.instance.RedeemPosition(ctx, fixture.accountID, fixture.position.ID)
	if err == nil {
		t.Fatal("an unknown receipt must be reported as an error")
	}
	if result.TxHash != hash {
		t.Fatalf("result lost the hash returned by the adapter: %q", result.TxHash)
	}
	op, ok, err := fixture.repository.ActiveChainOperation(ctx, fixture.accountID, market.VenuePredictFun, "redeem", &fixture.position.ID)
	if err != nil || !ok {
		t.Fatalf("active operation missing: ok=%v err=%v", ok, err)
	}
	if op.Status != "unknown" || op.TxHash != hash || op.FromAddress == "" {
		t.Fatalf("adapter hash was not journaled: %+v", op)
	}
}

func TestRedeemRevertedCompletesAsReverted(t *testing.T) {
	ctx := context.Background()
	fixture := newSettledChainFixture(t)
	hash := "0x" + strings.Repeat("2", 64)
	fixture.live.hash = hash
	fixture.live.err = &chainFailure{hash: hash, reverted: true}

	result, err := fixture.instance.RedeemPosition(ctx, fixture.accountID, fixture.position.ID)
	if err == nil {
		t.Fatal("a reverted redemption must be reported as an error")
	}
	if result.TxHash != hash {
		t.Fatalf("result lost the reverted hash: %q", result.TxHash)
	}
	if _, ok, err := fixture.repository.ActiveChainOperation(ctx, fixture.accountID, market.VenuePredictFun, "redeem", &fixture.position.ID); err != nil || ok {
		t.Fatalf("reverted operation must not stay active: ok=%v err=%v", ok, err)
	}
	op, ok, err := fixture.repository.ChainOperationByID(ctx, 1)
	if err != nil || !ok || op.Status != "reverted" || op.TxHash != hash {
		t.Fatalf("operation was not completed as reverted: %+v ok=%v err=%v", op, ok, err)
	}
	order := fixture.redeemOrder(t)
	if order.Status != "rejected" || !strings.Contains(order.Reason, hash) {
		t.Fatalf("reverted redemption order = %+v", order)
	}
}

func TestStaleIntentBecomesUnknownAndBlocksRedeem(t *testing.T) {
	ctx := context.Background()
	fixture := newSettledChainFixture(t)
	positionID := fixture.position.ID
	op, err := fixture.repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: fixture.accountID, Venue: market.VenuePredictFun, ChainID: 97, Kind: "redeem",
		PositionID: &positionID, MarketID: fixture.position.MarketID, TokenID: fixture.position.TokenID,
		FromAddress: fixture.live.Signer(), CreatedAt: time.Now().Add(-11 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := fixture.repository.ChainOperationByID(ctx, op.ID)
	if err != nil || !ok || stored.Status != "unknown" || stored.TxHash != "" ||
		stored.Reason != "进程中断，交易可能未广播，请核对钱包 nonce 与链上记录" {
		t.Fatalf("stale intent recovery = %+v ok=%v err=%v", stored, ok, err)
	}
	if _, err := fixture.instance.RedeemPosition(ctx, fixture.accountID, positionID); !errors.Is(err, engine.ErrChainOperationActive) {
		t.Fatalf("stale operation must block a second redeem, got %v", err)
	}
	if len(fixture.live.redemptions) != 0 {
		t.Fatalf("recovery must not broadcast a redemption: %d", len(fixture.live.redemptions))
	}
}

func TestRedeemRecoveryIsIdempotent(t *testing.T) {
	cases := []struct {
		name       string
		confirmed  bool
		reverted   bool
		statusErr  error
		wantOp     string
		wantOrder  string
		checkAgain bool
	}{
		{name: "confirmed", confirmed: true, wantOp: "confirmed", wantOrder: "filled", checkAgain: true},
		{name: "reverted", reverted: true, wantOp: "reverted", wantOrder: "rejected"},
		{name: "receipt missing", statusErr: errors.New("not found"), wantOp: "unknown", wantOrder: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newSettledChainFixture(t)
			hash := "0x" + strings.Repeat("3", 64)
			fixture.live.statusFn = func(string) (bool, bool, error) {
				return tc.confirmed, tc.reverted, tc.statusErr
			}
			order := fixture.pendingRedeemOrder(t)
			positionID := fixture.position.ID
			op, err := fixture.repository.BeginChainOperation(ctx, engine.ChainOperation{
				AccountID: fixture.accountID, Venue: market.VenuePredictFun, ChainID: 97, Kind: "redeem",
				PositionID: &positionID, MarketID: fixture.position.MarketID, TokenID: fixture.position.TokenID,
				FromAddress: fixture.live.Signer(),
			})
			if err != nil {
				t.Fatal(err)
			}
			nonce := uint64(4)
			if err := fixture.repository.SetChainOperationHash(ctx, op.ID, hash, fixture.live.Signer(), &nonce, 97); err != nil {
				t.Fatal(err)
			}
			if err := fixture.instance.ScanOnly(ctx); err != nil {
				t.Fatal(err)
			}
			stored, ok, err := fixture.repository.ChainOperationByID(ctx, op.ID)
			if err != nil || !ok || stored.Status != tc.wantOp || stored.TxHash != hash {
				t.Fatalf("operation after recovery = %+v ok=%v err=%v", stored, ok, err)
			}
			orders, err := fixture.instance.Orders(ctx, fixture.accountID, "all", 50, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, candidate := range orders.Items {
				if candidate.ID == order.ID {
					found = true
					if candidate.Status != tc.wantOrder {
						t.Fatalf("order after recovery = %+v, want %s", candidate, tc.wantOrder)
					}
				}
			}
			if !found {
				t.Fatal("redeem order disappeared")
			}
			if tc.checkAgain {
				calls := len(fixture.live.statusCalls)
				if err := fixture.instance.ScanOnly(ctx); err != nil {
					t.Fatal(err)
				}
				if len(fixture.live.statusCalls) != calls {
					t.Fatalf("terminal recovery was repeated: %d -> %d calls", calls, len(fixture.live.statusCalls))
				}
			}
		})
	}
}

type approvalLive struct {
	*fakeLive
	ready           bool
	redemptionReady *bool
	grantCalls      int
	stateCalls      int
	statusCalls     []string
	hashes          []string
}

func (a *approvalLive) ApprovalState(context.Context) (venue.Approval, error) {
	a.stateCalls++
	return venue.Approval{Venue: market.VenuePredictFun, Collateral: "USDT", Allowance: 10, SharesReady: a.ready, Ready: a.ready, RedemptionReady: a.redemptionReady}, nil
}

func (a *approvalLive) TransactionStatus(_ context.Context, hash string) (bool, bool, error) {
	a.statusCalls = append(a.statusCalls, hash)
	return false, false, errors.New("receipt not found")
}

func (a *approvalLive) GrantApprovals(_ context.Context, onSigned venue.ChainTxRecorder) (venue.Approval, error) {
	a.grantCalls++
	hash := "0x" + strings.Repeat("4", 64)
	if a.grantCalls > 1 {
		hash = "0x" + strings.Repeat("5", 64)
	}
	a.hashes = append(a.hashes, hash)
	if onSigned != nil {
		if err := onSigned(venue.ChainTxSigned{Hash: hash, From: a.Signer(), Nonce: uint64(a.grantCalls), ChainID: 97}); err != nil {
			return venue.Approval{}, err
		}
	}
	if a.grantCalls == 1 {
		// The first allowance is mined, then the batch stops before signing the
		// next transaction. The first row must remain confirmed.
		return venue.Approval{}, errors.New("later approval failed before signing")
	}
	return venue.Approval{Venue: market.VenuePredictFun, Collateral: "USDT", Allowance: 10, SharesReady: true, Ready: true}, nil
}

func TestApprovalPartialFailureRetryKeepsConfirmedRows(t *testing.T) {
	ctx := context.Background()
	provider := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{}}
	live := &approvalLive{fakeLive: &fakeLive{balance: 1000, allowance: 1000}}
	registry := venue.NewRegistry()
	addVenueCaps(t, registry, testCapabilities(market.VenuePredictFun), provider, live)
	instance, repository := newTestEngineWithRegistry(t, registry)

	if _, err := instance.ApproveVenue(ctx, market.VenuePredictFun); err == nil {
		t.Fatal("expected the first approval batch to stop")
	}
	first, ok, err := repository.ChainOperationByID(ctx, 1)
	if err != nil || !ok || first.Status != "confirmed" {
		t.Fatalf("completed approval was not kept confirmed: %+v ok=%v err=%v", first, ok, err)
	}
	if _, ok, err := repository.ActiveChainOperation(ctx, 0, market.VenuePredictFun, "approve", nil); err != nil || ok {
		t.Fatalf("a pre-sign failure must not leave an active lock: ok=%v err=%v", ok, err)
	}
	if _, err := instance.ApproveVenue(ctx, market.VenuePredictFun); err != nil {
		t.Fatalf("retry should only send the remaining approval: %v", err)
	}
	second, ok, err := repository.ChainOperationByID(ctx, 2)
	if err != nil || !ok || second.Status != "confirmed" || second.TxHash != live.hashes[1] {
		t.Fatalf("retry approval row = %+v ok=%v err=%v", second, ok, err)
	}
	if live.grantCalls != 2 || len(live.hashes) != 2 {
		t.Fatalf("approval batch calls = %d hashes=%v", live.grantCalls, live.hashes)
	}
}

func TestApprovalRecoveryUsesAllowanceWithoutReceipt(t *testing.T) {
	ctx := context.Background()
	provider := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{}}
	live := &approvalLive{fakeLive: &fakeLive{balance: 1000, allowance: 1000}, ready: true}
	registry := venue.NewRegistry()
	addVenueCaps(t, registry, testCapabilities(market.VenuePredictFun), provider, live)
	instance, repository := newTestEngineWithRegistry(t, registry)
	op, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: 0, Venue: market.VenuePredictFun, ChainID: 97, Kind: "approve",
		FromAddress: live.Signer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	nonce := uint64(3)
	if err := repository.SetChainOperationHash(ctx, op.ID, "0x"+strings.Repeat("6", 64), live.Signer(), &nonce, 97); err != nil {
		t.Fatal(err)
	}
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := repository.ChainOperationByID(ctx, op.ID)
	if err != nil || !ok || stored.Status != "confirmed" {
		t.Fatalf("allowance-based approval recovery = %+v ok=%v err=%v", stored, ok, err)
	}
	if len(live.statusCalls) != 0 {
		t.Fatalf("ready allowances must not trigger a receipt lookup: %v", live.statusCalls)
	}
}

func TestApprovalRecoveryWaitsForRedemptionReadiness(t *testing.T) {
	ctx := context.Background()
	provider := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{}}
	notReady := false
	live := &approvalLive{fakeLive: &fakeLive{balance: 1000, allowance: 1000}, ready: true, redemptionReady: &notReady}
	registry := venue.NewRegistry()
	addVenueCaps(t, registry, testCapabilities(market.VenuePredictFun), provider, live)
	instance, repository := newTestEngineWithRegistry(t, registry)
	op, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: 0, Venue: market.VenuePredictFun, ChainID: 97, Kind: "approve",
		FromAddress: live.Signer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	nonce := uint64(4)
	if err := repository.SetChainOperationHash(ctx, op.ID, "0x"+strings.Repeat("7", 64), live.Signer(), &nonce, 97); err != nil {
		t.Fatal(err)
	}
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := repository.ChainOperationByID(ctx, op.ID)
	if err != nil || !ok || stored.Status != "unknown" {
		t.Fatalf("unready redemption approval must stay unresolved: %+v ok=%v err=%v", stored, ok, err)
	}
	if len(live.statusCalls) != 1 {
		t.Fatalf("unready redemption approval should check its receipt: %v", live.statusCalls)
	}
}
