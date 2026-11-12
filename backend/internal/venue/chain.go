package venue

// ChainTxSigned identifies a transaction that has just been signed. It is
// emitted before the transaction is broadcast so a crash can never leave a
// chain transaction without a durable local identity.
type ChainTxSigned struct {
	// Hash is the transaction hash fixed by the signature.
	Hash string
	// From is the signing address.
	From string
	// Nonce is the account nonce used by the transaction. Zero is a valid
	// nonce, so callers that cannot read one should leave it at zero and treat
	// the stored value as informational.
	Nonce uint64
	// ChainID is the EIP-155 chain id the transaction was signed for. Zero
	// means the executor did not expose it.
	ChainID int64
}

// ChainTxRecorder persists a signed transaction before it is sent. Returning an
// error aborts the send: without a journal row the process must not create an
// on-chain transaction it cannot identify later.
type ChainTxRecorder func(ChainTxSigned) error
