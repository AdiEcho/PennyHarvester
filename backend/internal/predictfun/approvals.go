package predictfun

// This file holds the on-chain preparation a live Predict.fun order needs.
// Predict.fun uses USDT as collateral (an ERC-20 allowance) and Conditional
// Tokens as the outcome-share token (an ERC-1155 operator approval). The
// exchange family has two operator contracts, the plain exchange and the
// neg-risk exchange; both must be approved before trading so a neg-risk market
// cannot fail only after its order has already been signed and submitted.
//
// Approval transactions are deliberately explicit (EnsureApprovals), never a
// side effect of placing an order: sending a transaction spends BNB, can be
// slow, and must be visible to the operator.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	// minimumAllowance is one USDT in 18-decimal base units. A non-zero but
	// smaller allowance would still make a one-dollar order fail, so anything
	// below this is treated as "not approved".
	minimumAllowanceValue int64 = 1_000_000_000_000_000_000

	// approvalReceiptTimeout bounds one approval transaction wait. A sent
	// transaction can outlive this wait (see waitForReceipt), so the timeout is
	// only a reporting boundary, not a cancellation of the chain transaction.
	approvalReceiptTimeout = 3 * time.Minute
	// approvalPollInterval is how often the receipt is polled. BSC produces a
	// block roughly every three seconds, so two seconds is frequent enough
	// without hammering a public RPC.
	approvalPollInterval = 2 * time.Second
	// approvalGasLimitFallback is used when EstimateGas fails. An ERC-20
	// approve or ERC-1155 setApprovalForAll costs far less than 100k gas.
	approvalGasLimitFallback uint64 = 100_000
	// approvalGasPriceFloorWei is 1 gwei. BSC will not include a legacy
	// transaction paying less than the mempool floor, so a stale
	// SuggestGasPrice result is raised to this value instead of producing a
	// transaction that never confirms.
	approvalGasPriceFloorWei int64 = 1_000_000_000
)

// minimumAllowance is the comparison value for one USDT. It is a package
// variable only because big.Int cannot be a constant.
var minimumAllowance = big.NewInt(minimumAllowanceValue)

// maxUint256 is 2^256-1, the ERC-20 "unlimited" allowance. Approving this once
// avoids a new approval transaction for every order size.
var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// Function selectors are the first four bytes of keccak256(signature). They are
// fixed byte arrays so an encoder cannot accidentally append a variable-length
// string header.
var (
	selectorApprove           = [4]byte{0x09, 0x5e, 0xa7, 0xb3} // approve(address,uint256)
	selectorSetApprovalForAll = [4]byte{0xa2, 0x2c, 0xb4, 0x65} // setApprovalForAll(address,bool)
	selectorAllowance         = [4]byte{0xdd, 0x62, 0xed, 0x3e} // allowance(address,address)
	selectorBalanceOf         = [4]byte{0x70, 0xa0, 0x82, 0x31} // balanceOf(address)
	selectorApprovedForAll    = [4]byte{0xe9, 0x85, 0xe9, 0xc5} // isApprovedForAll(address,address)
)

// ApprovalState is the signer's collateral balance and the two approvals a
// live order needs. All amounts are 18-decimal USDT base units, never a
// float64: converting before a signed amount would round the value that the
// exchange checks.
type ApprovalState struct {
	// CollateralBalance is USDT balanceOf(signer).
	CollateralBalance *big.Int
	// ExchangeAllowance is the USDT allowance granted to the plain exchange.
	ExchangeAllowance *big.Int
	// NegRiskAllowance is the USDT allowance granted to the neg-risk exchange.
	NegRiskAllowance *big.Int
	// ExchangeApproved is ConditionalTokens.isApprovedForAll(signer, Exchange).
	ExchangeApproved bool
	// NegRiskApproved is ConditionalTokens.isApprovedForAll(signer, NegRiskExchange).
	NegRiskApproved bool
	// AdapterApproved is ConditionalTokens.isApprovedForAll(signer, NegRiskAdapter).
	// A neg-risk redemption is executed by the adapter, which burns the signer's
	// shares and therefore needs this operator approval. It gates redemption
	// only: trading works without it, so Ready (the trading check) ignores it.
	AdapterApproved bool
}

// CollateralAllowance returns the larger of the two USDT allowances. The two
// exchange operators pull collateral independently, so an order can only spend
// up to the allowance of the operator that settles it. Using the maximum keeps
// a funded neg-risk market usable when only that operator was approved; the
// per-operator checks in Ready still decide whether both are actually ready.
//
// The result is never nil, so callers can compare it without a nil check.
func (s ApprovalState) CollateralAllowance() *big.Int {
	result := new(big.Int)
	if s.ExchangeAllowance != nil {
		result.Set(s.ExchangeAllowance)
	}
	if s.NegRiskAllowance != nil && s.NegRiskAllowance.Cmp(result) > 0 {
		result.Set(s.NegRiskAllowance)
	}
	return result
}

// SharesApproved reports whether both exchange operators can move the signer's
// Conditional Tokens. An order signed for a neg-risk market is only valid after
// that operator is approved too; checking just one operator would let the order
// reach the exchange and fail there.
func (s ApprovalState) SharesApproved() bool {
	return s.ExchangeApproved && s.NegRiskApproved
}

// RedemptionApproved reports whether a neg-risk redemption may be attempted. The
// plain CTF path needs no approval at all, so a false value only blocks the
// markets the adapter settles.
func (s ApprovalState) RedemptionApproved() bool {
	return s.NegRiskApproved && s.AdapterApproved
}

// Ready reports whether the state can support an order needing at least
// minCollateral base units: the larger USDT allowance must cover it and both
// share operators must be approved. A nil minCollateral is treated as zero so a
// caller that only wants the share check can pass nil.
func (s ApprovalState) Ready(minCollateral *big.Int) bool {
	minimum := new(big.Int)
	if minCollateral != nil {
		minimum.Set(minCollateral)
	}
	return s.CollateralAllowance().Cmp(minimum) >= 0 && s.SharesApproved()
}

// OnChain performs the read-only allowance/balance calls and the explicit
// approval transactions. It is safe for concurrent use; the RPC client is
// created lazily and guarded by a mutex so constructing an OnChain (for
// example during startup or a credential probe) never opens a connection.
type OnChain struct {
	settings settings

	mu     sync.Mutex
	client *ethclient.Client

	// dial and sleep are test seams. Production uses ethclient.DialContext and
	// time.Sleep; a test can replace them without changing the call flow.
	dial  func(ctx context.Context, rawurl string) (*ethclient.Client, error)
	sleep func(time.Duration)
}

// NewOnChain resolves cfg and returns an on-chain helper. A missing private key
// is allowed: constructing the helper is also useful for read-only balance and
// allowance checks, and address reads report the missing key only when they are
// actually attempted. An invalid key or maker address is rejected with the same
// Chinese error resolveConfig produces.
func NewOnChain(cfg Config) (*OnChain, error) {
	resolved, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &OnChain{
		settings: resolved,
		dial:     ethclient.DialContext,
		sleep:    time.Sleep,
	}, nil
}

// ethClient returns the lazily created RPC client. Dialing is serialized so a
// burst of callers creates one client, not one per caller.
func (o *OnChain) ethClient(ctx context.Context) (*ethclient.Client, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.client != nil {
		return o.client, nil
	}
	dial := o.dial
	if dial == nil {
		dial = ethclient.DialContext
	}
	client, err := dial(ctx, o.settings.rpcURL)
	if err != nil {
		return nil, fmt.Errorf("连接 BSC RPC %s 失败: %w", o.settings.rpcURL, err)
	}
	o.client = client
	return client, nil
}

// Allowance reads the signer's USDT balance, both USDT allowances and both
// ConditionalTokens operator approvals with eth_call. It never sends a
// transaction or changes state.
func (o *OnChain) Allowance(ctx context.Context) (ApprovalState, error) {
	var state ApprovalState
	if o == nil {
		return state, fmt.Errorf("Predict.fun 链上授权客户端未初始化")
	}
	signer := o.settings.signer
	if signer == (common.Address{}) {
		return state, fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY，无法读取链上余额与授权")
	}
	client, err := o.ethClient(ctx)
	if err != nil {
		return state, err
	}

	usdt := common.HexToAddress(o.settings.addr.USDT)
	conditionalTokens := common.HexToAddress(o.settings.addr.ConditionalTokens)
	exchange := common.HexToAddress(o.settings.addr.Exchange)
	negRiskExchange := common.HexToAddress(o.settings.addr.NegRiskExchange)

	data, err := encodeBalanceOf(signer)
	if err != nil {
		return state, fmt.Errorf("读取 USDT 余额失败: %w", err)
	}
	state.CollateralBalance, err = readUint256(ctx, client, usdt, data)
	if err != nil {
		return state, fmt.Errorf("读取 USDT 余额失败: %w", err)
	}

	data, err = encodeAllowanceCall(signer, exchange)
	if err != nil {
		return state, fmt.Errorf("读取 USDT 授权额度失败: %w", err)
	}
	state.ExchangeAllowance, err = readUint256(ctx, client, usdt, data)
	if err != nil {
		return state, fmt.Errorf("读取 USDT 授权额度失败: %w", err)
	}

	data, err = encodeAllowanceCall(signer, negRiskExchange)
	if err != nil {
		return state, fmt.Errorf("读取 USDT 授权额度失败: %w", err)
	}
	state.NegRiskAllowance, err = readUint256(ctx, client, usdt, data)
	if err != nil {
		return state, fmt.Errorf("读取 USDT 授权额度失败: %w", err)
	}

	data, err = encodeApprovedForAll(signer, exchange)
	if err != nil {
		return state, fmt.Errorf("读取 ConditionalTokens 授权状态失败: %w", err)
	}
	state.ExchangeApproved, err = readBool(ctx, client, conditionalTokens, data)
	if err != nil {
		return state, fmt.Errorf("读取 ConditionalTokens 授权状态失败: %w", err)
	}

	data, err = encodeApprovedForAll(signer, negRiskExchange)
	if err != nil {
		return state, fmt.Errorf("读取 ConditionalTokens 授权状态失败: %w", err)
	}
	state.NegRiskApproved, err = readBool(ctx, client, conditionalTokens, data)
	if err != nil {
		return state, fmt.Errorf("读取 ConditionalTokens 授权状态失败: %w", err)
	}

	data, err = encodeApprovedForAll(signer, common.HexToAddress(o.settings.addr.NegRiskAdapter))
	if err != nil {
		return state, fmt.Errorf("读取 ConditionalTokens 授权状态失败: %w", err)
	}
	state.AdapterApproved, err = readBool(ctx, client, conditionalTokens, data)
	if err != nil {
		return state, fmt.Errorf("读取 ConditionalTokens 授权状态失败: %w", err)
	}

	return state, nil
}

// EnsureApprovals sends the approval transactions that Allowance reports as
// missing. It approves both the plain and the neg-risk operator for USDT and
// ConditionalTokens, skips transactions that are already satisfied, and sends
// them sequentially with local nonce increments.
//
// This is explicit and never called from an order path: a transaction costs
// BNB, may take minutes, and an order must not be delayed or silently made
// while the operator is waiting for a receipt.
func (o *OnChain) EnsureApprovals(ctx context.Context) error {
	if o == nil {
		return fmt.Errorf("Predict.fun 链上授权客户端未初始化")
	}
	if o.settings.key == nil || o.settings.signer == (common.Address{}) {
		return fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY，无法发送授权交易")
	}

	state, err := o.Allowance(ctx)
	if err != nil {
		return err
	}

	usdt := common.HexToAddress(o.settings.addr.USDT)
	conditionalTokens := common.HexToAddress(o.settings.addr.ConditionalTokens)
	operators := []struct {
		name      string
		address   common.Address
		allowance *big.Int
		approved  bool
		// collateral marks the operators that pull USDT for an order. The
		// neg-risk adapter only burns shares during a redemption, so granting it
		// a USDT allowance would widen the approval surface for nothing.
		collateral bool
	}{
		{
			name:       "Exchange",
			address:    common.HexToAddress(o.settings.addr.Exchange),
			allowance:  state.ExchangeAllowance,
			approved:   state.ExchangeApproved,
			collateral: true,
		},
		{
			name:       "NegRiskExchange",
			address:    common.HexToAddress(o.settings.addr.NegRiskExchange),
			allowance:  state.NegRiskAllowance,
			approved:   state.NegRiskApproved,
			collateral: true,
		},
		{
			name:     "NegRiskAdapter",
			address:  common.HexToAddress(o.settings.addr.NegRiskAdapter),
			approved: state.AdapterApproved,
		},
	}

	var calls []approvalCall
	for _, operator := range operators {
		allowance := operator.allowance
		if allowance == nil {
			allowance = new(big.Int)
		}
		if operator.collateral && allowance.Cmp(minimumAllowance) < 0 {
			data, encodeErr := encodeApprove(operator.address, maxUint256)
			if encodeErr != nil {
				return encodeErr
			}
			calls = append(calls, approvalCall{
				to:     usdt,
				data:   data,
				action: fmt.Sprintf("USDT approve(%s)", operator.name),
			})
		}
		if !operator.approved {
			data, encodeErr := encodeSetApprovalForAll(operator.address, true)
			if encodeErr != nil {
				return encodeErr
			}
			calls = append(calls, approvalCall{
				to:     conditionalTokens,
				data:   data,
				action: fmt.Sprintf("ConditionalTokens setApprovalForAll(%s)", operator.name),
			})
		}
	}
	if len(calls) == 0 {
		return nil
	}

	client, err := o.ethClient(ctx)
	if err != nil {
		return err
	}
	nonce, err := client.PendingNonceAt(ctx, o.settings.signer)
	if err != nil {
		return fmt.Errorf("读取待处理交易 nonce 失败: %w", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return fmt.Errorf("读取 BSC gas 价格失败: %w", err)
	}
	if gasPrice == nil {
		gasPrice = new(big.Int)
	}
	floor := big.NewInt(approvalGasPriceFloorWei)
	if gasPrice.Cmp(floor) < 0 {
		gasPrice = floor
	}

	// PendingNonceAt is only read once. Every following transaction must use the
	// next local nonce so a slow receipt cannot make two approvals reuse one
	// nonce and replace each other in the mempool.
	for index, call := range calls {
		if err := o.sendApproval(ctx, client, nonce+uint64(index), gasPrice, call); err != nil {
			return err
		}
	}
	return nil
}

// Balances returns the signer's USDT balance and the larger operator allowance
// in 18-decimal base units. It is the convenience form the executor uses for a
// CashSnapshot; the per-operator detail stays available through Allowance.
func (o *OnChain) Balances(ctx context.Context) (collateral *big.Int, allowance *big.Int, err error) {
	state, err := o.Allowance(ctx)
	if err != nil {
		return nil, nil, err
	}
	return state.CollateralBalance, state.CollateralAllowance(), nil
}

// approvalCall is one contract call that must be sent in order.
type approvalCall struct {
	to     common.Address
	data   []byte
	action string
	// gasLimitFallback replaces approvalGasLimitFallback for calls whose gas
	// use differs from an approval (a CTF redemption costs several times more).
	gasLimitFallback uint64
}

// sendApproval estimates, signs, sends and waits for one approval transaction.
func (o *OnChain) sendApproval(ctx context.Context, client *ethclient.Client, nonce uint64, gasPrice *big.Int, call approvalCall) error {
	_, err := o.sendCall(ctx, client, nonce, gasPrice, call)
	return err
}

// sendCall estimates, signs, sends and waits for one transaction, returning its
// hash. It never logs or returns the private key; the transaction hash is safe to
// report and is included in receipt failures.
func (o *OnChain) sendCall(ctx context.Context, client *ethclient.Client, nonce uint64, gasPrice *big.Int, call approvalCall) (common.Hash, error) {
	if err := ctx.Err(); err != nil {
		return common.Hash{}, fmt.Errorf("发送 %s 交易已取消: %w", call.action, err)
	}
	fallback := call.gasLimitFallback
	if fallback == 0 {
		fallback = approvalGasLimitFallback
	}

	to := call.to
	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{
		From:  o.settings.signer,
		To:    &to,
		Data:  call.data,
		Value: new(big.Int),
	})
	if err != nil || gasLimit == 0 {
		gasLimit = fallback
	} else {
		// 20% headroom absorbs a small state change between estimate and
		// inclusion (for example a cold allowance slot).
		gasLimit += gasLimit / 5
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: new(big.Int).Set(gasPrice),
		Gas:      gasLimit,
		To:       &to,
		Value:    new(big.Int),
		Data:     call.data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(o.settings.chainID)), o.settings.key)
	if err != nil {
		return common.Hash{}, fmt.Errorf("签名 %s 交易失败: %w", call.action, err)
	}
	if err := client.SendTransaction(ctx, signed); err != nil {
		return common.Hash{}, fmt.Errorf("发送 %s 交易失败: %w", call.action, err)
	}
	if err := o.waitForReceipt(ctx, client, signed.Hash()); err != nil {
		return signed.Hash(), err
	}
	return signed.Hash(), nil
}

// waitForReceipt polls for a receipt every two seconds for at most three
// minutes. If the wait expires, the transaction may still be included later:
// the timeout error carries the hash and the operator must check BscScan
// before retrying, because a retry after silent inclusion would send a second
// approval and spend gas again.
func (o *OnChain) waitForReceipt(ctx context.Context, client *ethclient.Client, hash common.Hash) error {
	waitCtx, cancel := context.WithTimeout(ctx, approvalReceiptTimeout)
	defer cancel()

	for {
		receipt, err := client.TransactionReceipt(waitCtx, hash)
		switch {
		case err == nil:
			if receipt == nil {
				return fmt.Errorf("链上交易 %s 没有返回回执", hash.Hex())
			}
			if receipt.Status == types.ReceiptStatusFailed {
				return fmt.Errorf("链上交易回滚：交易 %s 已失败（status=0），授权未生效，请检查 BNB 余额与授权合约后重试", hash.Hex())
			}
			return nil
		case !errors.Is(err, ethereum.NotFound):
			if waitCtx.Err() != nil {
				return receiptWaitError(ctx, hash)
			}
			return fmt.Errorf("查询链上交易 %s 回执失败: %w", hash.Hex(), err)
		}

		if waitCtx.Err() != nil {
			return receiptWaitError(ctx, hash)
		}
		sleep := o.sleep
		if sleep == nil {
			sleep = time.Sleep
		}
		sleep(approvalPollInterval)
	}
}

// receiptWaitError distinguishes a caller cancellation from the fixed
// three-minute receipt timeout.
func receiptWaitError(ctx context.Context, hash common.Hash) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("等待链上交易 %s 确认已取消: %w", hash.Hex(), err)
	}
	return fmt.Errorf("等待链上交易 %s 确认超时（3 分钟）：交易可能仍会确认，请到 BscScan 核对后再重试", hash.Hex())
}

// encodeApprove builds USDT.approve(spender, amount).
func encodeApprove(spender common.Address, amount *big.Int) ([]byte, error) {
	if amount == nil {
		return nil, fmt.Errorf("编码 USDT approve 调用失败：金额为空")
	}
	data, err := packCall(selectorApprove, []string{"address", "uint256"}, spender, amount)
	if err != nil {
		return nil, fmt.Errorf("编码 USDT approve 调用失败: %w", err)
	}
	return data, nil
}

// encodeSetApprovalForAll builds ConditionalTokens.setApprovalForAll(operator,
// approved).
func encodeSetApprovalForAll(operator common.Address, approved bool) ([]byte, error) {
	data, err := packCall(selectorSetApprovalForAll, []string{"address", "bool"}, operator, approved)
	if err != nil {
		return nil, fmt.Errorf("编码 ConditionalTokens setApprovalForAll 调用失败: %w", err)
	}
	return data, nil
}

// encodeAllowanceCall builds ERC20.allowance(owner, spender).
func encodeAllowanceCall(owner, spender common.Address) ([]byte, error) {
	data, err := packCall(selectorAllowance, []string{"address", "address"}, owner, spender)
	if err != nil {
		return nil, fmt.Errorf("编码 allowance 调用失败: %w", err)
	}
	return data, nil
}

// encodeBalanceOf builds ERC20.balanceOf(owner).
func encodeBalanceOf(owner common.Address) ([]byte, error) {
	data, err := packCall(selectorBalanceOf, []string{"address"}, owner)
	if err != nil {
		return nil, fmt.Errorf("编码 balanceOf 调用失败: %w", err)
	}
	return data, nil
}

// encodeApprovedForAll builds ERC1155.isApprovedForAll(owner, operator).
func encodeApprovedForAll(owner, operator common.Address) ([]byte, error) {
	data, err := packCall(selectorApprovedForAll, []string{"address", "address"}, owner, operator)
	if err != nil {
		return nil, fmt.Errorf("编码 isApprovedForAll 调用失败: %w", err)
	}
	return data, nil
}

// packCall ABI-packs the arguments and prefixes the selector. ABI encoding
// pads every static value to 32 bytes, which is what the contracts expect for
// these calls.
func packCall(selector [4]byte, typeNames []string, values ...any) ([]byte, error) {
	args := make(abi.Arguments, len(typeNames))
	for index, name := range typeNames {
		typ, err := abi.NewType(name, "", nil)
		if err != nil {
			return nil, fmt.Errorf("构建 ABI 类型 %s 失败: %w", name, err)
		}
		args[index] = abi.Argument{Name: fmt.Sprintf("arg%d", index), Type: typ}
	}
	packed, err := args.Pack(values...)
	if err != nil {
		return nil, fmt.Errorf("ABI 参数打包失败: %w", err)
	}
	data := make([]byte, 4+len(packed))
	copy(data, selector[:])
	copy(data[4:], packed)
	return data, nil
}

// readUint256 performs one eth_call and decodes a uint256 result.
func readUint256(ctx context.Context, client *ethclient.Client, target common.Address, data []byte) (*big.Int, error) {
	raw, err := client.CallContract(ctx, ethereum.CallMsg{To: &target, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return decodeUint256(raw)
}

// readBool performs one eth_call and decodes a bool result.
func readBool(ctx context.Context, client *ethclient.Client, target common.Address, data []byte) (bool, error) {
	raw, err := client.CallContract(ctx, ethereum.CallMsg{To: &target, Data: data}, nil)
	if err != nil {
		return false, err
	}
	return decodeBool(raw)
}

// decodeUint256 decodes one ABI-encoded uint256.
func decodeUint256(data []byte) (*big.Int, error) {
	typ, err := abi.NewType("uint256", "", nil)
	if err != nil {
		return nil, fmt.Errorf("构建 ABI 类型 uint256 失败: %w", err)
	}
	args := abi.Arguments{{Type: typ}}
	values, err := args.Unpack(data)
	if err != nil {
		return nil, fmt.Errorf("ABI 解码 uint256 失败: %w", err)
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("ABI 解码 uint256 返回 %d 个值，期望 1 个", len(values))
	}
	value, ok := values[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("ABI 解码 uint256 得到 %T，期望 *big.Int", values[0])
	}
	return new(big.Int).Set(value), nil
}

// decodeBool decodes one ABI-encoded bool.
func decodeBool(data []byte) (bool, error) {
	typ, err := abi.NewType("bool", "", nil)
	if err != nil {
		return false, fmt.Errorf("构建 ABI 类型 bool 失败: %w", err)
	}
	args := abi.Arguments{{Type: typ}}
	values, err := args.Unpack(data)
	if err != nil {
		return false, fmt.Errorf("ABI 解码 bool 失败: %w", err)
	}
	if len(values) != 1 {
		return false, fmt.Errorf("ABI 解码 bool 返回 %d 个值，期望 1 个", len(values))
	}
	value, ok := values[0].(bool)
	if !ok {
		return false, fmt.Errorf("ABI 解码 bool 得到 %T，期望 bool", values[0])
	}
	return value, nil
}
