package venue

import (
	"strings"
	"testing"
)

func TestCashSnapshotSelectsTheSettlingExchange(t *testing.T) {
	snapshot := CashSnapshot{Allowances: map[string]float64{
		PolymarketExchange:                           12.5,
		PolymarketNegRiskExchange:                    3,
		"0x1111111111111111111111111111111111111111": 99,
	}}
	if got, ok := snapshot.AllowanceFor(false); !ok || got != 12.5 {
		t.Fatalf("normal exchange allowance = %v, %v", got, ok)
	}
	if got, ok := snapshot.AllowanceFor(true); !ok || got != 3 {
		t.Fatalf("neg-risk exchange allowance = %v, %v", got, ok)
	}
	missing := CashSnapshot{Allowances: map[string]float64{
		"0x1111111111111111111111111111111111111111": 99,
	}}
	if got, ok := missing.AllowanceFor(false); !ok || got != 0 {
		t.Fatalf("an unrelated spender must not satisfy the target: %v, %v", got, ok)
	}
	if _, ok := (CashSnapshot{}).AllowanceFor(true); ok {
		t.Fatal("a legacy scalar snapshot has no per-spender evidence")
	}
}

// TestCashSnapshotSelectsExchangeV3ForProtocolV2 pins the V2 spender: the
// exchange is chosen by market version, not by the neg-risk flag, because
// ExchangeV3 settles both kinds of V2 market.
func TestCashSnapshotSelectsExchangeV3ForProtocolV2(t *testing.T) {
	snapshot := CashSnapshot{Allowances: map[string]float64{
		strings.ToLower(PolymarketExchangeV3): 7.25,
		PolymarketExchange:                    100,
		PolymarketNegRiskExchange:             100,
	}}
	for _, negRisk := range []bool{false, true} {
		got, ok := snapshot.AllowanceForProtocol("v2", negRisk)
		if !ok || got != 7.25 {
			t.Fatalf("v2 negRisk=%v allowance = %v, %v", negRisk, got, ok)
		}
	}
	if got, ok := snapshot.AllowanceForProtocol("v1", true); !ok || got != 100 {
		t.Fatalf("v1 must keep the CTF spender: %v, %v", got, ok)
	}
	if _, ok := (CashSnapshot{Allowances: map[string]float64{PolymarketExchange: 5}}).AllowanceForProtocol("v2", false); !ok {
		t.Fatal("a V2 snapshot with no ExchangeV3 entry has no evidence, not a free trade")
	}
	if got, ok := (CashSnapshot{Allowances: map[string]float64{PolymarketExchange: 5}}).AllowanceForProtocol("v2", false); ok && got != 0 {
		t.Fatalf("a missing ExchangeV3 approval must read as zero, got %v", got)
	}
}
