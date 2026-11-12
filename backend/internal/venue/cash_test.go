package venue

import "testing"

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
