package api

import (
	"fmt"
	"net/http"
	"testing"

	"pennyharvester/backend/internal/engine"
)

func TestChainOperationActiveMapsToConflict(t *testing.T) {
	wrapped := fmt.Errorf("redeem: %w", engine.ErrChainOperationActive)
	if got := redeemErrorStatus(wrapped); got != http.StatusConflict {
		t.Fatalf("redeem status = %d, want %d", got, http.StatusConflict)
	}
	if got := approvalErrorStatus(wrapped); got != http.StatusConflict {
		t.Fatalf("approval status = %d, want %d", got, http.StatusConflict)
	}
}
