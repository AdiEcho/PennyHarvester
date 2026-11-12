package predictfun

import (
	"crypto/ecdsa"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gomath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// orderType is the EIP-712 primary type of the order struct.
const orderType = "Order"

// Order is the EIP-712 order struct the exchange hashes. The field order and
// the field types are fixed: they are the hash preimage, so inserting, dropping
// or retyping a field produces a different hash and the exchange rejects the
// order after it has already left this process. Do not reorder.
//
// Taker is the zero address for a public order (the only kind this build
// places). Nonce is zero: the order hash already contains a random salt, the
// exchange deduplicates on that hash, and the SDK leaves nonce at zero for EOA
// orders. Neither field is used to express expiry; Expiration is
// now+FIVE_MINUTES_SECONDS, mirroring the SDK's GTD default.
type Order struct {
	Salt          *big.Int
	Maker         common.Address
	Signer        common.Address
	Taker         common.Address
	TokenID       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Expiration    *big.Int
	Nonce         *big.Int
	FeeRateBps    *big.Int
	Side          Side
	SignatureType SignatureType
}

// OrderDomain is the EIP-712 domain an order is signed against. ChainID decides
// the network; VerifyingContract decides the exchange family. Both are part of
// the hash: signing a neg-risk market against the plain exchange domain yields
// a perfectly valid signature for an order the exchange will never accept.
type OrderDomain struct {
	ChainID           *big.Int
	VerifyingContract common.Address
}

// ExchangeDomain picks the exchange contract for a market. Predict.fun routes
// negative-risk markets through a separate exchange, and a market flagged
// isYieldBearing through a third family this build refuses (see Provider).
func ExchangeDomain(chainID int64, addresses Addresses, negRisk bool) OrderDomain {
	contract := addresses.Exchange
	if negRisk {
		contract = addresses.NegRiskExchange
	}
	return OrderDomain{ChainID: big.NewInt(chainID), VerifyingContract: common.HexToAddress(contract)}
}

// TypedData renders the order as EIP-712 typed data. It is exported so a test
// (or a future signer) can inspect the exact struct that gets hashed.
func (o Order) TypedData(domain OrderDomain) (apitypes.TypedData, error) {
	if err := o.validate(domain); err != nil {
		return apitypes.TypedData{}, err
	}
	message := apitypes.TypedDataMessage{
		"salt":          bigOrZero(o.Salt),
		"maker":         o.Maker.Hex(),
		"signer":        o.Signer.Hex(),
		"taker":         o.Taker.Hex(),
		"tokenId":       bigOrZero(o.TokenID),
		"makerAmount":   bigOrZero(o.MakerAmount),
		"takerAmount":   bigOrZero(o.TakerAmount),
		"expiration":    bigOrZero(o.Expiration),
		"nonce":         bigOrZero(o.Nonce),
		"feeRateBps":    bigOrZero(o.FeeRateBps),
		"side":          new(big.Int).SetUint64(uint64(o.Side)),
		"signatureType": new(big.Int).SetUint64(uint64(o.SignatureType)),
	}
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			orderType: {
				{Name: "salt", Type: "uint256"},
				{Name: "maker", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "taker", Type: "address"},
				{Name: "tokenId", Type: "uint256"},
				{Name: "makerAmount", Type: "uint256"},
				{Name: "takerAmount", Type: "uint256"},
				{Name: "expiration", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "feeRateBps", Type: "uint256"},
				{Name: "side", Type: "uint8"},
				{Name: "signatureType", Type: "uint8"},
			},
		},
		PrimaryType: orderType,
		Domain: apitypes.TypedDataDomain{
			Name:              ProtocolName,
			Version:           ProtocolVersion,
			ChainId:           (*gomath.HexOrDecimal256)(new(big.Int).Set(domain.ChainID)),
			VerifyingContract: domain.VerifyingContract.Hex(),
		},
		Message: message,
	}, nil
}

// Hash returns the EIP-712 digest that is signed: keccak256(0x1901 ‖
// domainSeparator ‖ hashStruct(Order)).
func (o Order) Hash(domain OrderDomain) (common.Hash, error) {
	typedData, err := o.TypedData(domain)
	if err != nil {
		return common.Hash{}, err
	}
	digest, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		return common.Hash{}, fmt.Errorf("计算 Predict.fun 订单哈希: %w", err)
	}
	return common.BytesToHash(digest), nil
}

// Sign produces the 65-byte r‖s‖v signature for the order. go-ethereum returns
// v as 0/1, so 27 is added to return the canonical 27/28 form the exchange and
// every EVM signature checker expect.
//
// The returned hash is the order id this build uses before the API assigns one:
// the exchange deduplicates on it, so a resubmitted payload cannot become a
// second order.
func (o Order) Sign(key *ecdsa.PrivateKey, domain OrderDomain) (string, common.Hash, error) {
	if key == nil {
		return "", common.Hash{}, fmt.Errorf("缺少私钥，无法签名 Predict.fun 订单")
	}
	hash, err := o.Hash(domain)
	if err != nil {
		return "", common.Hash{}, err
	}
	signature, err := crypto.Sign(hash.Bytes(), key)
	if err != nil {
		return "", common.Hash{}, fmt.Errorf("签名 Predict.fun 订单: %w", err)
	}
	if len(signature) != crypto.SignatureLength {
		return "", common.Hash{}, fmt.Errorf("签名长度异常: %d", len(signature))
	}
	signature[64] += 27
	return hexutil.Encode(signature), hash, nil
}

// PersonalSign signs the login challenge with the EIP-191 personal message
// prefix. The API verifies this prefix, so an EIP-712 signature over the same
// text would be rejected.
func PersonalSign(key *ecdsa.PrivateKey, message string) (string, error) {
	if key == nil {
		return "", fmt.Errorf("缺少私钥，无法签名 Predict.fun 登录消息")
	}
	if message == "" {
		return "", fmt.Errorf("待签名消息为空")
	}
	prefixed := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	hash := crypto.Keccak256([]byte(prefixed))
	signature, err := crypto.Sign(hash, key)
	if err != nil {
		return "", fmt.Errorf("签名 Predict.fun 登录消息: %w", err)
	}
	signature[64] += 27
	return hexutil.Encode(signature), nil
}

// RandomSalt draws a salt in [0, MaxSalt), matching the SDK's bound. The salt is
// what makes two otherwise identical orders distinct, so it must come from a
// CSPRNG and not from a counter or the clock.
func RandomSalt() (*big.Int, error) {
	salt, err := rand.Int(rand.Reader, big.NewInt(MaxSalt))
	if err != nil {
		return nil, fmt.Errorf("生成 Predict.fun 订单随机盐: %w", err)
	}
	return salt, nil
}

// validate rejects the states that would produce a signature the exchange
// cannot accept, before anything is sent. It deliberately does not require
// Taker or Expiration to be non-zero: both have documented zero values.
func (o Order) validate(domain OrderDomain) error {
	if domain.ChainID == nil || domain.ChainID.Sign() <= 0 {
		return fmt.Errorf("EIP-712 域缺少有效的 chainId")
	}
	if domain.VerifyingContract == (common.Address{}) {
		return fmt.Errorf("EIP-712 域缺少校验合约地址")
	}
	if o.Maker == (common.Address{}) || o.Signer == (common.Address{}) {
		return fmt.Errorf("订单缺少 maker 或 signer")
	}
	if o.Salt != nil && (o.Salt.Sign() < 0 || o.Salt.Cmp(big.NewInt(MaxSalt)) >= 0) {
		return fmt.Errorf("订单随机盐超出 [0,%d) 范围", MaxSalt)
	}
	if o.Side != SideBuy && o.Side != SideSell {
		return fmt.Errorf("订单方向无效: %d", o.Side)
	}
	if o.SignatureType != SignatureTypeEOA {
		return fmt.Errorf("仅支持 EOA 订单签名（signatureType=0）")
	}
	if o.TokenID == nil || o.TokenID.Sign() <= 0 {
		return fmt.Errorf("订单缺少有效的 tokenId")
	}
	for name, value := range map[string]*big.Int{
		"makerAmount": o.MakerAmount,
		"takerAmount": o.TakerAmount,
		"nonce":       o.Nonce,
		"feeRateBps":  o.FeeRateBps,
	} {
		if value == nil {
			continue
		}
		if value.Sign() < 0 {
			return fmt.Errorf("订单字段 %s 不能为负", name)
		}
	}
	if o.Expiration != nil {
		if o.Expiration.Sign() < 0 {
			return fmt.Errorf("订单字段 expiration 不能为负")
		}
		if !o.Expiration.IsInt64() {
			return fmt.Errorf("订单字段 expiration 超出 int64 Unix 秒范围")
		}
	}
	return nil
}

func bigOrZero(value *big.Int) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return value
}

// signedOrder converts the in-memory struct into the wire form. Every integer
// becomes a decimal string, so a 77-digit token id and an 18-decimal amount
// cross the JSON boundary without rounding. It lives here so the signed form
// and the wire form are defined next to each other.
func (o Order) signedOrder(signature string) SignedOrder {
	return SignedOrder{
		Salt:          bigOrZero(o.Salt).String(),
		Maker:         o.Maker.Hex(),
		Signer:        o.Signer.Hex(),
		Taker:         o.Taker.Hex(),
		TokenID:       bigOrZero(o.TokenID).String(),
		MakerAmount:   bigOrZero(o.MakerAmount).String(),
		TakerAmount:   bigOrZero(o.TakerAmount).String(),
		Expiration:    bigOrZero(o.Expiration).Int64(),
		Nonce:         bigOrZero(o.Nonce).String(),
		FeeRateBps:    bigOrZero(o.FeeRateBps).String(),
		Side:          uint8(o.Side),
		SignatureType: uint8(o.SignatureType),
		Signature:     strings.TrimSpace(signature),
	}
}
