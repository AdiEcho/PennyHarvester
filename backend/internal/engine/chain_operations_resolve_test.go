package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

func beginUnknownRedeemOperation(t *testing.T, fixture *settledChainFixture, hash string) engine.ChainOperation {
	t.Helper()
	ctx := context.Background()
	positionID := fixture.position.ID
	operation, err := fixture.repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: fixture.accountID, Venue: market.VenuePredictFun, ChainID: 97, Kind: "redeem",
		PositionID: &positionID, MarketID: fixture.position.MarketID, TokenID: fixture.position.TokenID,
		FromAddress: "0x0000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		if err := fixture.repository.SetChainOperationHash(ctx, operation.ID, hash, operation.FromAddress, nil, 97); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.repository.FinishChainOperation(ctx, operation.ID, "unknown", "等待运营核对链上记录"); err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestManualChainOperationResolutionUpdatesLinkedRedeemOrder(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		resolution  string
		orderStatus string
	}{
		{"confirmed", "filled"},
		{"reverted", "rejected"},
	} {
		t.Run(test.resolution, func(t *testing.T) {
			fixture := newSettledChainFixture(t)
			order := fixture.pendingRedeemOrder(t)
			hash := "0x" + strings.Repeat("4", 64)
			operation := beginUnknownRedeemOperation(t, fixture, hash)

			note := "运营核对链上回执：" + test.resolution
			resolved, err := fixture.instance.ResolveChainOperation(ctx, operation.ID, test.resolution, note, "ops@example.com", "")
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Status != test.resolution || resolved.ResolvedBy != "ops@example.com" || resolved.ResolutionNote != note {
				t.Fatalf("resolution audit was not returned: %+v", resolved)
			}
			if resolved.ResolvedAt == nil {
				t.Fatalf("resolved_at must be set: %+v", resolved)
			}
			if resolved.TxHash != hash {
				t.Fatalf("existing hash must be preserved, got %q", resolved.TxHash)
			}

			storedOrder := fixture.redeemOrder(t)
			if storedOrder.ID != order.ID || storedOrder.Status != test.orderStatus {
				t.Fatalf("linked redeem order was not updated: got id=%d status=%q, want id=%d status=%q", storedOrder.ID, storedOrder.Status, order.ID, test.orderStatus)
			}
			if storedOrder.Reason != note {
				t.Fatalf("linked order reason should be the resolution note, got %q", storedOrder.Reason)
			}

			listed, err := fixture.instance.ListChainOperations(ctx, engine.ChainOperationFilter{
				AccountID: fixture.accountID, Venue: market.VenuePredictFun, Status: "all",
			})
			if err != nil {
				t.Fatal(err)
			}
			if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != operation.ID || listed.Items[0].Status != test.resolution {
				t.Fatalf("engine list did not return the resolved operation: %+v", listed)
			}
		})
	}
}

func TestManualChainOperationResolutionRefusesNonUnknownRow(t *testing.T) {
	ctx := context.Background()
	fixture := newSettledChainFixture(t)
	order := fixture.pendingRedeemOrder(t)
	hash := "0x" + strings.Repeat("5", 64)
	positionID := fixture.position.ID
	intent, err := fixture.repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: fixture.accountID, Venue: market.VenuePredictFun, ChainID: 97, Kind: "redeem",
		PositionID: &positionID, MarketID: fixture.position.MarketID, TokenID: fixture.position.TokenID,
		FromAddress: "0x0000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.instance.ResolveChainOperation(ctx, intent.ID, "confirmed", "手工确认", "ops", hash); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("intent rows are recovery's job and must not be manually resolved, got %v", err)
	}
	if stored := fixture.redeemOrder(t); stored.ID != order.ID || stored.Status != "pending" {
		t.Fatalf("a refused resolution must not touch the linked order: %+v", stored)
	}

	if _, err := fixture.instance.ListChainOperations(ctx, engine.ChainOperationFilter{Status: "settled"}); !errors.Is(err, engine.ErrChainOperationInvalid) {
		t.Fatalf("invalid list status must be rejected, got %v", err)
	}
	if _, err := fixture.instance.ListChainOperations(ctx, engine.ChainOperationFilter{Offset: -1}); !errors.Is(err, engine.ErrChainOperationInvalid) {
		t.Fatalf("negative list offset must be rejected, got %v", err)
	}
	if _, err := fixture.instance.ResolveChainOperation(ctx, intent.ID, "unknown", "x", "ops", ""); !errors.Is(err, engine.ErrChainOperationInvalid) {
		t.Fatalf("manual resolution must reject status unknown, got %v", err)
	}
}
