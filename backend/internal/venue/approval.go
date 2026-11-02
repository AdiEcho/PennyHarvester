package venue

import (
	"context"

	"pennyharvester/backend/internal/market"
)

// Approval is the on-chain allowance state a venue needs before it can trade.
// It is venue-neutral so the UI can render one card without knowing which chain
// or token the venue settles in.
type Approval struct {
	Venue      market.Venue `json:"venue"`
	Collateral string       `json:"collateral"`
	// Balance is the wallet's collateral balance.
	Balance float64 `json:"balance"`
	// Allowance is the collateral amount the venue may pull.
	Allowance float64 `json:"allowance"`
	// SharesReady reports whether outcome shares may be transferred (an ERC1155
	// operator approval). Polymarket grants this through its proxy; Predict.fun
	// needs it before a sell can settle.
	SharesReady bool `json:"shares_ready"`
	// Ready is true when nothing has to be granted any more.
	Ready bool `json:"ready"`
	// Missing names what still has to be approved, in the user's language.
	Missing string `json:"missing,omitempty"`
}

// Approver is implemented by an executor whose venue requires on-chain
// allowances. A venue that trades through a custodian or proxy does not
// implement it, and the engine then reports that no approval is needed.
type Approver interface {
	// ApprovalState reads the current allowances. It is read-only.
	ApprovalState(ctx context.Context) (Approval, error)
	// GrantApprovals sends the missing approval transactions and returns the
	// resulting state. It spends gas from the venue's own wallet.
	GrantApprovals(ctx context.Context) (Approval, error)
}
