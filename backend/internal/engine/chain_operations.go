package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/venue"
)

// A chain operation is the durable identity of one signed transaction. The
// orders table is the UI ledger; this table is the crash journal. They are
// deliberately separate because an approval has no order, and a redemption can
// need recovery long after its order has been rendered.
type ChainOperation struct {
	ID             int64        `json:"id"`
	AccountID      int64        `json:"account_id"`
	Venue          market.Venue `json:"venue"`
	ChainID        int64        `json:"chain_id"`
	Kind           string       `json:"kind"`
	PositionID     *int64       `json:"position_id,omitempty"`
	MarketID       string       `json:"market_id,omitempty"`
	TokenID        string       `json:"token_id,omitempty"`
	FromAddress    string       `json:"from_address"`
	Nonce          *uint64      `json:"nonce,omitempty"`
	TxHash         string       `json:"tx_hash,omitempty"`
	Status         string       `json:"status"`
	Reason         string       `json:"reason,omitempty"`
	ResolvedBy     string       `json:"resolved_by,omitempty"`
	ResolutionNote string       `json:"resolution_note,omitempty"`
	ResolvedAt     *time.Time   `json:"resolved_at,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// ChainOperationFilter selects a deterministic page of the chain journal. Zero
// values mean "all": account 0 spans every account, an empty venue spans every
// venue, and an empty status spans every state.
type ChainOperationFilter struct {
	AccountID int64
	Venue     market.Venue
	Status    string
	Limit     int
	Offset    int
}

// ChainOperationPage is a full result page. Total counts every matching row, not
// just the returned slice, so the UI can paginate an unresolved backlog.
type ChainOperationPage struct {
	Items  []ChainOperation `json:"items"`
	Total  int64            `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

const (
	// venueApprovalAccountID attributes a venue-wide approval. Approval
	// transactions authorize the venue's wallet, and the approval API has no
	// account parameter; zero keeps the uniqueness key venue-wide instead of
	// changing meaning when the default account is switched.
	venueApprovalAccountID int64 = 0

	chainOperationStaleAfter     = 10 * time.Minute
	chainOperationWriteTimeout   = 5 * time.Second
	chainOperationRecoveryLimit  = 50
	chainOperationRecoveryBudget = 12 * time.Second
	chainOperationRecoveryPerOp  = 5 * time.Second

	// Manual resolution is an operator action, so every field that proves who
	// closed an unknown row is bounded before it becomes an audit record.
	chainOperationActorMax  = 120
	chainOperationReasonMax = 500
	chainOperationTxHashLen = 66 // 0x + 64 hex characters
)

const chainOperationStaleReason = "进程中断，交易可能未广播，请核对钱包 nonce 与链上记录"

func chainIDOf(exec venue.Executor) int64 {
	if exec == nil {
		return 0
	}
	if provider, ok := exec.(interface{ ChainID() int64 }); ok {
		return provider.ChainID()
	}
	return 0
}

func (e *Engine) beginChainOperation(ctx context.Context, accountID int64, v market.Venue, chainID int64, kind, from string, positionID *int64, marketID, tokenID string) (ChainOperation, error) {
	from = strings.TrimSpace(from)
	if from == "" {
		return ChainOperation{}, fmt.Errorf("%w: %s 未返回签名地址", ErrVenueNotConfigured, v.Label())
	}
	now := time.Now().UTC()
	return e.repo.BeginChainOperation(ctx, ChainOperation{
		AccountID:   accountID,
		Venue:       v,
		ChainID:     chainID,
		Kind:        kind,
		PositionID:  positionID,
		MarketID:    marketID,
		TokenID:     tokenID,
		FromAddress: from,
		Status:      "intent",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

func (e *Engine) recordChainHash(ctx context.Context, id int64, hash, from string, nonce *uint64, chainID int64) error {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return errors.New("签名后的交易哈希为空，拒绝广播")
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chainOperationWriteTimeout)
	defer cancel()
	return e.repo.SetChainOperationHash(writeCtx, id, hash, strings.TrimSpace(from), nonce, chainID)
}

func (e *Engine) finishChainOperation(ctx context.Context, id int64, status, reason string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chainOperationWriteTimeout)
	defer cancel()
	return e.repo.FinishChainOperation(writeCtx, id, status, clampReason(reason))
}

// ListChainOperations is the operator's read path over the chain journal. It
// validates the filter here as well as in the store so a caller cannot ask for
// a venue or state the schema does not contain. Limit 0 means the default and a
// larger value is capped rather than rejected: a list endpoint must stay
// bounded even when a caller asks for everything.
func (e *Engine) ListChainOperations(ctx context.Context, filter ChainOperationFilter) (ChainOperationPage, error) {
	filter.Status = normalizeChainOperationStatus(filter.Status)
	if filter.AccountID < 0 {
		return ChainOperationPage{}, fmt.Errorf("%w: account_id 不能为负数", ErrChainOperationInvalid)
	}
	if filter.Venue != "" && !filter.Venue.Valid() {
		return ChainOperationPage{}, fmt.Errorf("%w: 未知场所 %q", ErrChainOperationInvalid, filter.Venue)
	}
	if filter.Status != "" && !validChainOperationStatus(filter.Status) {
		return ChainOperationPage{}, fmt.Errorf("%w: 无效的链上操作状态 %q", ErrChainOperationInvalid, filter.Status)
	}
	if filter.Limit < 0 {
		return ChainOperationPage{}, fmt.Errorf("%w: limit 不能为负数", ErrChainOperationInvalid)
	}
	if filter.Offset < 0 {
		return ChainOperationPage{}, fmt.Errorf("%w: offset 不能为负数", ErrChainOperationInvalid)
	}
	return e.repo.ListChainOperations(ctx, filter)
}

// ResolveChainOperation closes an unknown row with an operator's verified
// outcome. It takes tradeMu so a manual resolution cannot interleave with a
// live redemption or the recovery pass updating the same linked order. It never
// touches the chain: no RPC, no signature, no new nonce.
func (e *Engine) ResolveChainOperation(ctx context.Context, id int64, status, reason, actor, txHash string) (ChainOperation, error) {
	status = strings.TrimSpace(status)
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if id <= 0 {
		return ChainOperation{}, fmt.Errorf("%w: 链上操作 ID 无效", ErrChainOperationInvalid)
	}
	if status != "confirmed" && status != "reverted" {
		return ChainOperation{}, fmt.Errorf("%w: 状态仅支持 confirmed 或 reverted", ErrChainOperationInvalid)
	}
	if actor == "" || len([]rune(actor)) > chainOperationActorMax {
		return ChainOperation{}, fmt.Errorf("%w: 操作人必填且不超过 %d 个字符", ErrChainOperationInvalid, chainOperationActorMax)
	}
	if reason == "" || len([]rune(reason)) > chainOperationReasonMax {
		return ChainOperation{}, fmt.Errorf("%w: 核对说明必填且不超过 %d 个字符", ErrChainOperationInvalid, chainOperationReasonMax)
	}
	hash, err := normalizeChainOperationTxHash(txHash)
	if err != nil {
		return ChainOperation{}, err
	}

	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	current, ok, err := e.repo.ChainOperationByID(ctx, id)
	if err != nil {
		return ChainOperation{}, err
	}
	if !ok {
		return ChainOperation{}, engineNotFound("链上操作")
	}
	// Resolve the journal row and its linked redemption order atomically. The
	// order lookup happens here because only the engine knows the redeem-intent
	// rule; the store applies both writes in one transaction.
	var orderID int64
	var orderStatus string
	if current.Kind == "redeem" && current.PositionID != nil {
		if intent, found, err := e.repo.RedeemIntent(ctx, current.AccountID, *current.PositionID); err != nil {
			return ChainOperation{}, err
		} else if found {
			orderID = intent.ID
			switch status {
			case "confirmed":
				orderStatus = "filled"
			case "reverted":
				orderStatus = "rejected"
			}
		}
	}
	resolved, err := e.repo.ResolveChainOperationAndOrder(ctx, id, status, reason, actor, hash, orderID, orderStatus)
	if err != nil {
		return ChainOperation{}, err
	}
	return resolved, nil
}

// engineNotFound keeps the store's ErrNotFound sentinel while naming the
// resource in the message.
func engineNotFound(resource string) error {
	return fmt.Errorf("%w: %s不存在", ErrNotFound, resource)
}

func normalizeChainOperationStatus(raw string) string {
	status := strings.TrimSpace(raw)
	if strings.EqualFold(status, "all") {
		return ""
	}
	return status
}

func validChainOperationStatus(status string) bool {
	switch status {
	case "intent", "signed", "broadcast", "confirmed", "reverted", "unknown":
		return true
	default:
		return false
	}
}

func normalizeChainOperationTxHash(raw string) (string, error) {
	hash := strings.TrimSpace(raw)
	if hash == "" {
		return "", nil
	}
	if len(hash) != chainOperationTxHashLen || hash[0] != '0' || (hash[1] != 'x' && hash[1] != 'X') {
		return "", fmt.Errorf("%w: 交易哈希须为 0x 加 64 位十六进制", ErrChainOperationInvalid)
	}
	for _, digit := range hash[2:] {
		if !(digit >= '0' && digit <= '9') && !(digit >= 'a' && digit <= 'f') && !(digit >= 'A' && digit <= 'F') {
			return "", fmt.Errorf("%w: 交易哈希须为 0x 加 64 位十六进制", ErrChainOperationInvalid)
		}
	}
	return strings.ToLower(hash), nil
}

// transactionHashError attaches the signed hash to an error from a layer that
// does not know the hash. The API uses the same interface as the venue adapters,
// so it keeps returning tx_hash without importing Predict.fun.
type transactionHashError struct {
	err  error
	hash string
}

func (e *transactionHashError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *transactionHashError) Unwrap() error { return e.err }

func (e *transactionHashError) TransactionHash() string {
	if e == nil {
		return ""
	}
	return e.hash
}

func withTransactionHash(err error, hash string) error {
	if err == nil || hash == "" || transactionHashOf(err) != "" {
		return err
	}
	return &transactionHashError{err: err, hash: hash}
}

func transactionRevertedOf(err error) bool {
	var reverted interface{ TransactionReverted() bool }
	return errors.As(err, &reverted) && reverted.TransactionReverted()
}

// approvalJournal records one chain operation per approval transaction. The
// adapter signs and waits for each transaction sequentially, so a callback for
// transaction N proves transaction N-1 was confirmed. Finishing N-1 at that
// point keeps a partially completed batch restartable without sending a second
// transaction for an allowance that already exists.
type approvalJournal struct {
	engine  *Engine
	ctx     context.Context
	venue   market.Venue
	exec    venue.Executor
	current *ChainOperation
	from    string
	chainID int64
}

func newApprovalJournal(ctx context.Context, e *Engine, v market.Venue, exec venue.Executor) *approvalJournal {
	journal := &approvalJournal{engine: e, ctx: ctx, venue: v, exec: exec}
	if exec != nil {
		journal.from = exec.Signer()
		journal.chainID = chainIDOf(exec)
	}
	return journal
}

func (j *approvalJournal) recorder(tx venue.ChainTxSigned) error {
	if strings.TrimSpace(tx.Hash) == "" {
		return errors.New("签名后的交易哈希为空，拒绝广播")
	}
	if j.current != nil {
		if err := j.engine.finishChainOperation(j.ctx, j.current.ID, "confirmed", "链上授权交易已确认"); err != nil {
			return err
		}
		j.current = nil
	}
	from := strings.TrimSpace(tx.From)
	if from == "" {
		from = j.from
	}
	chainID := tx.ChainID
	if chainID == 0 {
		chainID = j.chainID
	}
	op, err := j.engine.beginChainOperation(j.ctx, venueApprovalAccountID, j.venue, chainID, "approve", from, nil, "", "")
	if err != nil {
		return err
	}
	nonce := tx.Nonce
	if err := j.engine.recordChainHash(j.ctx, op.ID, tx.Hash, from, &nonce, chainID); err != nil {
		_ = j.engine.finishChainOperation(j.ctx, op.ID, "reverted", "签名后未广播，授权交易未发送")
		return err
	}
	j.current = &op
	return nil
}

// fail finishes the transaction the journal currently owns. A hash means the
// node may have accepted it, so the row stays active (unknown) and no retry may
// use a fresh nonce. Without a hash the callback contract proves the send did
// not happen.
func (j *approvalJournal) fail(cause error) string {
	hash := transactionHashOf(cause)
	if hash != "" && j.current == nil {
		// The adapter reported a signed hash without journaling it first. Keep
		// the identity anyway: an unrecorded signature must not be retried with
		// a fresh nonce.
		op, err := j.engine.beginChainOperation(j.ctx, venueApprovalAccountID, j.venue, j.chainID, "approve", j.from, nil, "", "")
		if err == nil {
			if err := j.engine.recordChainHash(j.ctx, op.ID, hash, j.from, nil, j.chainID); err == nil {
				_ = j.engine.finishChainOperation(j.ctx, op.ID, "unknown", "广播或回执结果未知，交易 "+hash+"，请先核对链上状态")
			}
		}
	}
	if hash == "" && j.current != nil {
		// The adapter sends sequentially and waits for each receipt. A failure
		// without a hash therefore happened before the next signature, so the
		// journal's current row is the previous, already mined transaction.
		_ = j.engine.finishChainOperation(j.ctx, j.current.ID, "confirmed", "链上授权交易已确认")
		j.current = nil
		return ""
	}
	if j.current != nil {
		status := "reverted"
		reason := "签名或广播前失败，授权交易未发送"
		switch {
		case transactionRevertedOf(cause):
			reason = "链上授权交易已回滚"
			if hash != "" {
				reason += " " + hash
			}
		case hash != "":
			status = "unknown"
			reason = "广播或回执结果未知，交易 " + hash + "，请先核对链上状态"
		}
		_ = j.engine.finishChainOperation(j.ctx, j.current.ID, status, reason)
		j.current = nil
	}
	return hash
}

func (j *approvalJournal) succeed() {
	if j.current == nil {
		return
	}
	_ = j.engine.finishChainOperation(j.ctx, j.current.ID, "confirmed", "链上授权交易已确认")
	j.current = nil
}

func (e *Engine) recoverChainOperations(ctx context.Context) {
	recoveryCtx, cancel := context.WithTimeout(ctx, chainOperationRecoveryBudget)
	defer cancel()
	ops, err := e.repo.ListUnresolvedChainOperations(recoveryCtx, chainOperationRecoveryLimit)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, op := range ops {
		if recoveryCtx.Err() != nil {
			return
		}
		opCtx, opCancel := context.WithTimeout(recoveryCtx, chainOperationRecoveryPerOp)
		e.recoverChainOperation(opCtx, op, now)
		opCancel()
	}
}

// recoverChainOperation resolves one active row without guessing. It runs
// outside tradeMu so a slow RPC cannot hold every account scan.
func (e *Engine) recoverChainOperation(ctx context.Context, op ChainOperation, now time.Time) {
	if op.TxHash == "" {
		if op.CreatedAt.IsZero() || now.Sub(op.CreatedAt) < chainOperationStaleAfter {
			return
		}
		e.applyChainRecovery(func() {
			if op.Kind == "redeem" {
				e.markRedeemOrder(ctx, op, "unknown", chainOperationStaleReason)
			}
			_ = e.finishChainOperation(ctx, op.ID, "unknown", chainOperationStaleReason)
		})
		return
	}

	exec, err := e.executor(op.Venue)
	if err != nil {
		return
	}
	switch op.Kind {
	case "approve":
		approver, ok := exec.(venue.Approver)
		if !ok {
			return
		}
		approval, approvalErr := approver.ApprovalState(ctx)
		ready := approvalErr == nil && approval.Ready
		if ready && approval.RedemptionReady != nil {
			ready = *approval.RedemptionReady
		}
		if ready {
			e.applyChainRecovery(func() {
				_ = e.finishChainOperation(ctx, op.ID, "confirmed", "链上授权已生效")
			})
			return
		}
		confirmed, reverted, supported, statusErr := transactionStatusOf(ctx, exec, op.TxHash)
		if !supported {
			return
		}
		if statusErr != nil {
			e.applyChainRecovery(func() {
				_ = e.finishChainOperation(ctx, op.ID, "unknown", "查询链上交易回执失败，交易 "+op.TxHash+"，请先核对链上状态")
			})
			return
		}
		switch {
		case reverted:
			e.applyChainRecovery(func() {
				_ = e.finishChainOperation(ctx, op.ID, "reverted", "链上授权交易已回滚 "+op.TxHash)
			})
		case confirmed:
			// An interrupted batch can leave earlier transactions confirmed
			// while later allowances are still missing. The receipt resolves
			// this row; a retry then starts from the first missing allowance.
			e.applyChainRecovery(func() {
				_ = e.finishChainOperation(ctx, op.ID, "confirmed", "链上授权交易已确认 "+op.TxHash)
			})
		}
	case "redeem":
		confirmed, reverted, supported, statusErr := transactionStatusOf(ctx, exec, op.TxHash)
		if !supported {
			return
		}
		if statusErr != nil {
			reason := "广播或回执结果未知，交易 " + op.TxHash + "，请先核对链上状态"
			e.applyChainRecovery(func() {
				e.markRedeemOrder(ctx, op, "unknown", reason)
				_ = e.finishChainOperation(ctx, op.ID, "unknown", reason)
			})
			return
		}
		switch {
		case reverted:
			reason := "链上赎回交易已回滚，交易 " + op.TxHash
			e.applyChainRecovery(func() {
				e.markRedeemOrder(ctx, op, "rejected", reason)
				_ = e.finishChainOperation(ctx, op.ID, "reverted", reason)
			})
		case confirmed:
			e.applyChainRecovery(func() {
				e.markRedeemOrder(ctx, op, "filled", "链上赎回交易已提交 "+op.TxHash)
				_ = e.finishChainOperation(ctx, op.ID, "confirmed", "链上赎回交易已确认 "+op.TxHash)
			})
		}
	}
}

// applyChainRecovery applies a decision after all RPC calls have finished. The
// trade mutex is only taken briefly for the local writes, and a busy mutex means
// a live approval/redemption owns the row: skip it rather than racing that
// request or making scan wait for its chain receipts.
func (e *Engine) applyChainRecovery(apply func()) {
	if !e.tradeMu.TryLock() {
		return
	}
	defer e.tradeMu.Unlock()
	apply()
}

func transactionStatusOf(ctx context.Context, exec venue.Executor, hash string) (confirmed, reverted, supported bool, err error) {
	reader, ok := exec.(interface {
		TransactionStatus(context.Context, string) (bool, bool, error)
	})
	if !ok {
		return false, false, false, nil
	}
	confirmed, reverted, err = reader.TransactionStatus(ctx, hash)
	return confirmed, reverted, true, err
}

func (e *Engine) markRedeemOrder(ctx context.Context, op ChainOperation, status, reason string) {
	if op.PositionID == nil {
		return
	}
	intent, ok, err := e.repo.RedeemIntent(ctx, op.AccountID, *op.PositionID)
	if err != nil || !ok {
		return
	}
	_ = e.repo.MarkOrderStatus(ctx, intent.ID, status, clampReason(reason))
}
