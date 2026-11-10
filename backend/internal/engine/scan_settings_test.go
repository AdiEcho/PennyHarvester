package engine_test

import (
	"context"
	"testing"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

// The scan cadence belongs to the process, not to an account. A stale
// per-account value must never be what the scheduler or the settings page sees.
func TestAccountScanIntervalCannotOverrideGlobal(t *testing.T) {
	ctx := context.Background()
	instance, repository := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)

	global := instance.Settings()
	global.ScanIntervalSeconds = 10
	if err := instance.UpdateSettings(ctx, global); err != nil {
		t.Fatal(err)
	}

	custom, err := instance.SettingsFor(ctx, paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	custom.ScanIntervalSeconds = 600
	custom.OrderSize = 7
	if err := instance.UpdateAccountSettings(ctx, paper.ID, custom); err != nil {
		t.Fatal(err)
	}

	stored, err := instance.SettingsFor(ctx, paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ScanIntervalSeconds != 10 {
		t.Fatalf("account read must expose the global scan interval, got %d", stored.ScanIntervalSeconds)
	}
	if stored.OrderSize != 7 {
		t.Fatalf("the rest of the account strategy must still save: %+v", stored)
	}

	persisted, err := repository.LoadAccountSettings(ctx, paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ScanIntervalSeconds != 10 {
		t.Fatalf("the persisted account block must not keep a dead scan interval, got %d", persisted.ScanIntervalSeconds)
	}

	// A later global change takes effect immediately for the account read. The
	// stored account row is only rewritten when the account is saved again.
	global.ScanIntervalSeconds = 30
	if err := instance.UpdateSettings(ctx, global); err != nil {
		t.Fatal(err)
	}
	stored, err = instance.SettingsFor(ctx, paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ScanIntervalSeconds != 30 {
		t.Fatalf("account read must follow the current global scan interval, got %d", stored.ScanIntervalSeconds)
	}
}
