// Package order signs Polymarket CLOB orders for both protocol generations.
//
// CTF (v1) markets settle on the CTF Exchange V2 with EIP-712 domain version
// "2". Protocol V2 markets settle on ExchangeV3 with domain version "3". The
// order struct is identical; only the domain differs, so one signer serves
// both and the caller selects the domain from the market's Gamma version.
//
// The exchange accepts the signature only when the digest is byte-identical to
// what its own Order hash produces. Amounts are therefore computed exactly the
// way Polymarket's clients do (see Amounts) and the digest is built from the
// published Order type, never from a rounded or JSON-encoded form.
package order

import (
	"crypto/ecdsa"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// OrderTypeString is the EIP-712 primary type. Its field order is consensus:
// changing it changes every signature.
const OrderTypeString = "Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)"

// Poly1271TypeString wraps an order signature for deposit wallets. The Order
// field list is appended verbatim; the exchange reconstructs it the same way.
const Poly1271TypeString = "TypedDataSign(Order contents,string name,string version,uint256 chainId,address verifyingContract,bytes32 salt)" + OrderTypeString

// Addresses fixed by Polymarket's deployment. They are constants, not
// configuration: a typo here produces a valid signature for an exchange that
// will never see the order.
var (
	// ExchangeV3 settles every Protocol V2 market.
	ExchangeV3 = common.HexToAddress("0xe3333700cA9d93003F00f0F71f8515005F6c00Aa")
	// PositionManager holds Protocol V2 outcome shares as ERC-1155 tokens.
	PositionManager = common.HexToAddress("0x006F54F7f9A22e0000CC2AB60031000000ae9fEF")
	// CTFExchangeV2 settles legacy binary markets.
	CTFExchangeV2 = common.HexToAddress("0xE111180000d2663C0091e4f400237545B87B996B")
	// NegRiskExchangeV2 settles legacy neg-risk markets.
	NegRiskExchangeV2 = common.HexToAddress("0xe2222d279d744050d28e00520010520000310F59")
)

// ChainPolygonMainnet is the only chain Polymarket's production CLOB signs
// for.
const ChainPolygonMainnet int64 = 137

// Domain is one EIP-712 domain. Name and Version are part of the digest; a
// mismatch yields a signature the exchange cannot attribute.
type Domain struct {
	Name              string
	Version           string
	ChainID           int64
	VerifyingContract common.Address
}

// DomainFor selects the domain from the market protocol version. version is
// the Gamma market version ("v2" for Protocol V2); negRisk only affects the
// legacy CTF exchange and is ignored for Protocol V2, which has one exchange.
func DomainFor(version string, negRisk bool) (Domain, error) {
	d := Domain{
		Name:    "Polymarket CTF Exchange",
		ChainID: ChainPolygonMainnet,
	}
	switch {
	case IsProtocolV2(version):
		d.Version = "3"
		d.VerifyingContract = ExchangeV3
	case strings.EqualFold(strings.TrimSpace(version), "v1"), strings.TrimSpace(version) == "":
		d.Version = "2"
		d.VerifyingContract = CTFExchangeV2
		if negRisk {
			d.VerifyingContract = NegRiskExchangeV2
		}
	default:
		return Domain{}, fmt.Errorf("unsupported market protocol version %q", version)
	}
	return d, nil
}

// IsProtocolV2 reports whether a Gamma market version names Protocol V2.
func IsProtocolV2(version string) bool {
	return strings.EqualFold(strings.TrimSpace(version), "v2")
}

// Order is the signed payload. Expiration is deliberately absent: it travels
// in the wire body but is not part of the EIP-712 struct, and adding it here
// would change every digest.
type Order struct {
	Salt          int64
	Maker         common.Address
	Signer        common.Address
	TokenID       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Side          uint8
	SignatureType uint8
	Timestamp     int64
	Metadata      common.Hash
	Builder       common.Hash
}

// Signed carries the digest (which the CLOB uses as the order ID) and the
// 0x-prefixed signature bytes.
type Signed struct {
	Order     Order
	Digest    common.Hash
	Signature string
}

// OrderID is the digest hex, matching the ID the CLOB returns for the order.
func (s Signed) OrderID() string { return s.Digest.Hex() }

var (
	uint256ABI = mustABIType("uint256")
	uint8ABI   = mustABIType("uint8")
	addressABI = mustABIType("address")
	bytes32ABI = mustABIType("bytes32")

	orderArgs = abi.Arguments{
		{Type: bytes32ABI},
		{Type: uint256ABI},
		{Type: addressABI},
		{Type: addressABI},
		{Type: uint256ABI},
		{Type: uint256ABI},
		{Type: uint256ABI},
		{Type: uint8ABI},
		{Type: uint8ABI},
		{Type: uint256ABI},
		{Type: bytes32ABI},
		{Type: bytes32ABI},
	}
	domainArgs = abi.Arguments{
		{Type: bytes32ABI},
		{Type: bytes32ABI},
		{Type: bytes32ABI},
		{Type: uint256ABI},
		{Type: addressABI},
	}
	poly1271Args = abi.Arguments{
		{Type: bytes32ABI},
		{Type: bytes32ABI},
		{Type: bytes32ABI},
		{Type: bytes32ABI},
		{Type: uint256ABI},
		{Type: addressABI},
		{Type: bytes32ABI},
	}

	domainTypeHash    = crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	orderTypeHash     = crypto.Keccak256Hash([]byte(OrderTypeString))
	poly1271TypeHash  = crypto.Keccak256Hash([]byte(Poly1271TypeString))
	depositWalletName = crypto.Keccak256Hash([]byte("DepositWallet"))
	depositWalletVer  = crypto.Keccak256Hash([]byte("1"))
	depositWalletSalt = common.Hash{}
)

func mustABIType(name string) abi.Type {
	typ, err := abi.NewType(name, "", nil)
	if err != nil {
		panic("polymarket order: bad ABI type " + name + ": " + err.Error())
	}
	return typ
}

// Separator computes the domain separator. It is exposed so startup can
// compare it with ExchangeV3.domainSeparator() before any order is signed.
func (d Domain) Separator() common.Hash {
	encoded, err := domainArgs.Pack(
		domainTypeHash,
		crypto.Keccak256Hash([]byte(d.Name)),
		crypto.Keccak256Hash([]byte(d.Version)),
		big.NewInt(d.ChainID),
		d.VerifyingContract,
	)
	if err != nil {
		// The argument types are fixed at package init, so a pack failure here
		// is impossible and must not silently produce a zero domain.
		panic("polymarket order: domain pack: " + err.Error())
	}
	return crypto.Keccak256Hash(encoded)
}

// StructHash computes the EIP-712 struct hash of the order.
func (o Order) StructHash() (common.Hash, error) {
	if o.TokenID == nil || o.MakerAmount == nil || o.TakerAmount == nil {
		return common.Hash{}, fmt.Errorf("order token and amounts are required")
	}
	encoded, err := orderArgs.Pack(
		orderTypeHash,
		big.NewInt(o.Salt),
		o.Maker,
		o.Signer,
		o.TokenID,
		o.MakerAmount,
		o.TakerAmount,
		o.Side,
		o.SignatureType,
		big.NewInt(o.Timestamp),
		o.Metadata,
		o.Builder,
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("encode order: %w", err)
	}
	return crypto.Keccak256Hash(encoded), nil
}

// Digest computes the EIP-712 digest the exchange verifies.
func Digest(domain Domain, o Order) (common.Hash, error) {
	structHash, err := o.StructHash()
	if err != nil {
		return common.Hash{}, err
	}
	separator := domain.Separator()
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, separator[:], structHash[:]), nil
}

// Sign validates the order, then produces its digest and signature. Signature
// type 3 (POLY_1271) wraps the inner signature as the deposit-wallet exchange
// expects; every other type signs the bare digest.
func Sign(key *ecdsa.PrivateKey, domain Domain, o Order) (Signed, error) {
	if key == nil {
		return Signed{}, fmt.Errorf("private key is required to sign an order")
	}
	if o.Maker == (common.Address{}) || o.Signer == (common.Address{}) {
		return Signed{}, fmt.Errorf("order maker and signer are required")
	}
	if o.TokenID == nil || o.TokenID.Sign() <= 0 {
		return Signed{}, fmt.Errorf("order token ID must be positive")
	}
	if o.MakerAmount == nil || o.TakerAmount == nil || o.MakerAmount.Sign() <= 0 || o.TakerAmount.Sign() <= 0 {
		return Signed{}, fmt.Errorf("order amounts must be positive")
	}
	if o.Side > 1 {
		return Signed{}, fmt.Errorf("order side must be 0 (buy) or 1 (sell)")
	}
	if o.SignatureType > 3 {
		return Signed{}, fmt.Errorf("unsupported signature type %d", o.SignatureType)
	}
	if o.SignatureType == 3 && o.Signer != o.Maker {
		return Signed{}, fmt.Errorf("POLY_1271 orders must name the deposit wallet as both maker and signer")
	}

	structHash, err := o.StructHash()
	if err != nil {
		return Signed{}, err
	}
	separator := domain.Separator()
	digest := crypto.Keccak256Hash([]byte{0x19, 0x01}, separator[:], structHash[:])

	if o.SignatureType != 3 {
		sig, err := crypto.Sign(digest[:], key)
		if err != nil {
			return Signed{}, fmt.Errorf("sign order: %w", err)
		}
		sig[64] += 27
		return Signed{Order: o, Digest: digest, Signature: hexutil.Encode(sig)}, nil
	}
	signature, err := poly1271Signature(key, separator, o, structHash)
	if err != nil {
		return Signed{}, err
	}
	return Signed{Order: o, Digest: digest, Signature: signature}, nil
}

// poly1271Signature builds the ERC-1271 envelope: the 65-byte inner signature
// over the wrapped digest, followed by the domain separator, contents hash,
// contents type string and its length. ExchangeV3 hashes the same envelope to
// call isValidSignature on the deposit wallet.
func poly1271Signature(key *ecdsa.PrivateKey, separator common.Hash, o Order, contentsHash common.Hash) (string, error) {
	outerHash, err := poly1271StructHash(separator, o, contentsHash)
	if err != nil {
		return "", err
	}
	inner, err := crypto.Sign(outerHash[:], key)
	if err != nil {
		return "", fmt.Errorf("sign POLY_1271 order: %w", err)
	}
	inner[64] += 27
	contentsType := hexutil.Encode([]byte(OrderTypeString))[2:]
	return "0x" + hexutil.Encode(inner)[2:] +
		hexutil.Encode(separator[:])[2:] +
		hexutil.Encode(contentsHash[:])[2:] +
		contentsType +
		fmt.Sprintf("%04x", len(OrderTypeString)), nil
}

func poly1271StructHash(separator common.Hash, o Order, contentsHash common.Hash) (common.Hash, error) {
	encoded, err := poly1271Args.Pack(
		poly1271TypeHash,
		contentsHash,
		depositWalletName,
		depositWalletVer,
		big.NewInt(ChainPolygonMainnet),
		o.Maker,
		depositWalletSalt,
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("encode POLY_1271 order: %w", err)
	}
	typedHash := crypto.Keccak256Hash(encoded)
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, separator[:], typedHash[:]), nil
}

// Amounts computes maker and taker base-unit amounts the way Polymarket does:
// round to 6 decimals, then floor the buyer's collateral to 0.01 USDC and the
// buyer's shares to 0.0001 (the opposite for sells and for non-immediate
// orders). The exchange rejects any other rounding at signature time.
func Amounts(side string, price, shares float64, orderType string) (maker, taker *big.Int, err error) {
	if price <= 0 || price >= 1 || math.IsNaN(price) || math.IsInf(price, 0) {
		return nil, nil, fmt.Errorf("price %.8f is outside (0, 1)", price)
	}
	if shares <= 0 || math.IsNaN(shares) || math.IsInf(shares, 0) {
		return nil, nil, fmt.Errorf("shares %.8f must be positive", shares)
	}
	side = strings.ToUpper(strings.TrimSpace(side))
	if side != "BUY" && side != "SELL" {
		return nil, nil, fmt.Errorf("side %q must be BUY or SELL", side)
	}
	priceRat := decimalRat(price)
	sharesRat := decimalRat(shares)
	collateral := roundScaled(new(big.Rat).Mul(priceRat, sharesRat), 1_000_000)
	shareUnits := roundScaled(sharesRat, 1_000_000)
	immediate := side == "BUY" && (orderType == "FOK" || orderType == "FAK")
	if immediate {
		floorToMultiple(collateral, 10_000)
		floorToMultiple(shareUnits, 100)
	} else {
		floorToMultiple(collateral, 100)
		floorToMultiple(shareUnits, 10_000)
	}
	if collateral.Sign() <= 0 || shareUnits.Sign() <= 0 {
		return nil, nil, fmt.Errorf("order amounts rounded to zero")
	}
	if side == "BUY" {
		return collateral, shareUnits, nil
	}
	return shareUnits, collateral, nil
}

func decimalRat(value float64) *big.Rat {
	rat, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
	if !ok {
		return new(big.Rat)
	}
	return rat
}

func roundScaled(value *big.Rat, scale int64) *big.Int {
	scaled := new(big.Rat).Mul(value, new(big.Rat).SetInt64(scale))
	num := new(big.Int).Set(scaled.Num())
	den := scaled.Denom()
	quotient, remainder := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(remainder, big.NewInt(2)).Cmp(den) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

func floorToMultiple(value *big.Int, multiple int64) {
	divisor := big.NewInt(multiple)
	value.Div(value, divisor).Mul(value, divisor)
}
