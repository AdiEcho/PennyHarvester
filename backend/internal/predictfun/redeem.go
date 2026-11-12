package predictfun

// This file implements the on-chain half of a settlement: burning resolved
// outcome shares back into USDT. It is the only place in this build that sends a
// transaction whose purpose is to receive money, so the encoding is built from
// inspected constants instead of being hand-assembled in place, and every path
// is covered by a calldata test.
//
// Two contract shapes exist (verified against the SDK source and the live API):
//
//   - plain markets: ConditionalTokens.redeemPositions(
//     address collateralToken, bytes32 parentCollectionId, bytes32 conditionId,
//     uint256[] indexSets) with parentCollectionId = 0x00..00 and indexSets
//     built from the market's own outcomes[].indexSet (1 = Yes/first,
//     2 = No/second). Selector 0x01b7037c.
//   - neg-risk markets: NegRiskAdapter.redeemPositions(bytes32 conditionId,
//     uint256[] amounts) where amounts is per outcome slot and only the slot
//     being burned carries a value ("[amt, 0]" for Yes, "[0, amt]" for No).
//     Selector 0xdbeccb23.
//
// Redemption costs BNB for gas; there is no relayer or gasless path for it, so
// the sender refuses early with an explicit message instead of failing inside the
// mempool.

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/venue"
)

// RedeemMethod names the contract path taken, for the API response and support.
const (
	RedeemMethodCTF     = "ctf"
	RedeemMethodNegRisk = "neg-risk"
)

// redeemMinGasBalanceWei is the balance below which a redemption is refused
// before signing. One redeemPositions call costs roughly 100k gas; at 1 gwei
// that is 1e14 wei, so 0.0001 BNB is a floor with two orders of magnitude of
// headroom, not a precise fee estimate.
const redeemMinGasBalanceWei int64 = 100_000_000_000_000

// redeemGasLimitFallback is used when EstimateGas fails. A CTF redemption for a
// binary market is far below this.
const redeemGasLimitFallback uint64 = 400_000

// Function selectors are derived from their signatures instead of being typed
// in: a typo in the signature string cannot silently produce a call to a
// different function. TestRedeemSelectors pins the result to the published
// values.
var (
	selectorRedeemCTF     = selectorOf("redeemPositions(address,bytes32,bytes32,uint256[])")
	selectorRedeemNegRisk = selectorOf("redeemPositions(bytes32,uint256[])")
)

// selectorOf is the first four bytes of keccak256(signature).
func selectorOf(signature string) [4]byte {
	digest := crypto.Keccak256([]byte(signature))
	var selector [4]byte
	copy(selector[:], digest[:4])
	return selector
}

// redeemPlan is one encoded redemption call.
type redeemPlan struct {
	to     common.Address
	data   []byte
	method string
}

// planRedeem builds the call for one outcome slot. indexSet is 1 for the first
// outcome (Yes/Up) and 2 for the second (No/Down); shares is in 18-decimal
// collateral base units.
func planRedeem(collateral, conditionalTokens, negRiskAdapter common.Address, negRisk bool, conditionID common.Hash, indexSet int, shares *big.Int) (redeemPlan, error) {
	if shares == nil || shares.Sign() <= 0 {
		return redeemPlan{}, fmt.Errorf("赎回数量必须大于 0")
	}
	if indexSet != 1 && indexSet != 2 {
		return redeemPlan{}, fmt.Errorf("赎回结果序号 %d 无效（二元市场只有 1 或 2）", indexSet)
	}
	if negRisk {
		// amounts is per outcome slot and only the burned slot carries a value.
		amounts := []*big.Int{new(big.Int), new(big.Int)}
		amounts[indexSet-1] = new(big.Int).Set(shares)
		return redeemPlan{
			to:     negRiskAdapter,
			data:   encodeTwoArgUintArrayCall(selectorRedeemNegRisk, conditionID, amounts),
			method: RedeemMethodNegRisk,
		}, nil
	}
	return redeemPlan{
		to:     conditionalTokens,
		data:   encodeRedeemPositionsCTF(collateral, conditionID, indexSet),
		method: RedeemMethodCTF,
	}, nil
}

// encodeRedeemPositionsCTF encodes
// redeemPositions(address,bytes32,bytes32,uint256[]).
func encodeRedeemPositionsCTF(collateral common.Address, conditionID common.Hash, indexSet int) []byte {
	var data []byte
	data = append(data, selectorRedeemCTF[:]...)
	data = append(data, padAddress(collateral)...)
	// parentCollectionId is the zero hash for a top-level condition, which is
	// every market this build trades.
	data = append(data, make([]byte, 32)...)
	data = append(data, conditionID.Bytes()...)
	data = append(data, padUint(big.NewInt(0x80))...) // offset of indexSets
	data = append(data, padUint(big.NewInt(1))...)    // length
	data = append(data, padUint(big.NewInt(int64(indexSet)))...)
	return data
}

// encodeTwoArgUintArrayCall encodes f(bytes32,uint256[]) — the NegRiskAdapter
// shape. The head is the condition id followed by the array offset (0x40).
func encodeTwoArgUintArrayCall(selector [4]byte, conditionID common.Hash, values []*big.Int) []byte {
	var data []byte
	data = append(data, selector[:]...)
	data = append(data, conditionID.Bytes()...)
	data = append(data, padUint(big.NewInt(0x40))...)
	data = append(data, padUint(big.NewInt(int64(len(values))))...)
	for _, value := range values {
		data = append(data, padUint(value)...)
	}
	return data
}

// padAddress left-pads a 20-byte address to one ABI word.
func padAddress(address common.Address) []byte {
	word := make([]byte, 32)
	copy(word[12:], address.Bytes())
	return word
}

// padUint left-pads a uint256 to one ABI word.
func padUint(value *big.Int) []byte {
	word := make([]byte, 32)
	if value == nil {
		return word
	}
	bytes := value.Bytes()
	if len(bytes) > 32 {
		bytes = bytes[len(bytes)-32:]
	}
	copy(word[32-len(bytes):], bytes)
	return word
}

// parseConditionID accepts the 0x-prefixed 32-byte condition id the API
// publishes and refuses anything else: a truncated id would redeem nothing and
// still cost gas.
func parseConditionID(raw string) (common.Hash, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "0x"), "0X")
	if len(trimmed) != 64 {
		return common.Hash{}, fmt.Errorf("条件 ID %q 不是 32 字节十六进制", raw)
	}
	decoded, err := hex.DecodeString(trimmed)
	if err != nil {
		return common.Hash{}, fmt.Errorf("条件 ID %q 不是合法的十六进制: %w", raw, err)
	}
	return common.BytesToHash(decoded), nil
}

// RedeemRequest is the on-chain redemption input. Shares is in 18-decimal
// collateral base units; the caller converts from the human amount so the value
// that reaches the chain is the one that was checked.
type RedeemRequest struct {
	ConditionID string
	IndexSet    int
	Shares      *big.Int
	NegRisk     bool
}

// RedeemTx is the submitted transaction.
type RedeemTx struct {
	Hash   common.Hash
	Method string
}

// Redeem burns the resolved shares held for one condition and waits for the
// receipt. It refuses before signing when the signer cannot pay for gas, because
// a transaction stuck in the mempool is worse than a clear error.
func (o *OnChain) Redeem(ctx context.Context, request RedeemRequest, onSigned venue.ChainTxRecorder) (RedeemTx, error) {
	conditionID, err := parseConditionID(request.ConditionID)
	if err != nil {
		return RedeemTx{}, err
	}
	plan, err := planRedeem(
		common.HexToAddress(o.settings.addr.USDT),
		common.HexToAddress(o.settings.addr.ConditionalTokens),
		common.HexToAddress(o.settings.addr.NegRiskAdapter),
		request.NegRisk, conditionID, request.IndexSet, request.Shares,
	)
	if err != nil {
		return RedeemTx{}, err
	}
	client, err := o.ethClient(ctx)
	if err != nil {
		return RedeemTx{}, err
	}
	// A neg-risk redemption is executed by the NegRiskAdapter, which burns the
	// signer's shares and therefore needs the CTF operator approval. Checking it
	// first turns a guaranteed revert (paid for in gas) into an instruction.
	if request.NegRisk {
		state, stateErr := o.Allowance(ctx)
		if stateErr != nil {
			return RedeemTx{}, stateErr
		}
		if !state.RedemptionApproved() {
			return RedeemTx{}, fmt.Errorf("neg-risk 市场赎回需要先授权 NegRiskAdapter 划转结果份额，请在「系统设置 → Predict.fun」执行链上授权")
		}
	}
	balance, err := client.BalanceAt(ctx, o.settings.signer, nil)
	if err != nil {
		return RedeemTx{}, fmt.Errorf("读取 BNB 余额失败: %w", err)
	}
	if balance == nil || balance.Cmp(big.NewInt(redeemMinGasBalanceWei)) < 0 {
		return RedeemTx{}, fmt.Errorf("赎回需要在 BSC 上支付 gas，但签名地址 %s 的 BNB 余额不足（当前 %s wei），请先充值",
			o.settings.signer.Hex(), weiText(balance))
	}
	nonce, err := client.PendingNonceAt(ctx, o.settings.signer)
	if err != nil {
		return RedeemTx{}, fmt.Errorf("读取待处理交易 nonce 失败: %w", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return RedeemTx{}, fmt.Errorf("读取 BSC gas 价格失败: %w", err)
	}
	if gasPrice == nil {
		gasPrice = new(big.Int)
	}
	if floor := big.NewInt(approvalGasPriceFloorWei); gasPrice.Cmp(floor) < 0 {
		gasPrice = floor
	}
	call := approvalCall{
		to:               plan.to,
		data:             plan.data,
		action:           "赎回 " + plan.method,
		gasLimitFallback: redeemGasLimitFallback,
	}
	hash, err := o.sendCall(ctx, client, nonce, gasPrice, call, onSigned)
	if err != nil {
		// A hash is returned only after signing and the journal callback
		// accepted it; keep it so callers can report an unknown broadcast
		// instead of an anonymous failure. A callback rejection returns no
		// hash because nothing was broadcast.
		return RedeemTx{Hash: hash, Method: plan.method}, err
	}
	return RedeemTx{Hash: hash, Method: plan.method}, nil
}

// weiText renders a wei balance without exploding on nil.
func weiText(balance *big.Int) string {
	if balance == nil {
		return "0"
	}
	return balance.String()
}
