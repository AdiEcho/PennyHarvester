package predictfun

import (
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// fixedOrder is a deterministic order used to prove that the EIP-712 encoding
// is self-consistent: if a field type or the domain were wrong, the recovered
// address would still be some address, but any test that pins the signer would
// catch a change to the hash preimage.
func fixedOrder(t *testing.T, key *ecdsa.PrivateKey) Order {
	t.Helper()
	signer := crypto.PubkeyToAddress(key.PublicKey)
	return Order{
		Salt:          big.NewInt(1_234_567),
		Maker:         signer,
		Signer:        signer,
		Taker:         common.Address{},
		TokenID:       big.NewInt(987_654_321),
		MakerAmount:   mustBig(t, "5000000000000000000"),
		TakerAmount:   mustBig(t, "10000000000000000000"),
		Expiration:    big.NewInt(1_900_000_000),
		Nonce:         new(big.Int),
		FeeRateBps:    big.NewInt(200),
		Side:          SideBuy,
		SignatureType: SignatureTypeEOA,
	}
}

// mustBig parses a decimal string into a big.Int, failing the test on garbage.
func mustBig(t *testing.T, value string) *big.Int {
	t.Helper()
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		t.Fatalf("invalid integer fixture %q", value)
	}
	return parsed
}

func TestSignOrderRecoversSigner(t *testing.T) {
	key := testKey(t)
	order := fixedOrder(t, key)
	addresses, ok := AddressesFor(ChainIDBnbTestnet)
	if !ok {
		t.Fatal("testnet addresses missing")
	}
	domain := ExchangeDomain(ChainIDBnbTestnet, addresses, false)

	signature, hash, err := order.Sign(key, domain)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, err := hexutil.Decode(signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(raw) != 65 {
		t.Fatalf("signature length = %d, want 65", len(raw))
	}
	if raw[64] != 27 && raw[64] != 28 {
		t.Fatalf("signature v = %d, want 27 or 28", raw[64])
	}
	raw[64] -= 27
	publicKey, err := crypto.Ecrecover(hash.Bytes(), raw)
	if err != nil {
		t.Fatalf("Ecrecover: %v", err)
	}
	recovered, err := crypto.UnmarshalPubkey(publicKey)
	if err != nil {
		t.Fatalf("UnmarshalPubkey: %v", err)
	}
	if got, want := crypto.PubkeyToAddress(*recovered), crypto.PubkeyToAddress(key.PublicKey); got != want {
		t.Fatalf("recovered signer = %s, want %s", got, want)
	}
}

func TestOrderHashDependsOnDomain(t *testing.T) {
	key := testKey(t)
	order := fixedOrder(t, key)
	mainnet, ok := AddressesFor(ChainIDBnbMainnet)
	if !ok {
		t.Fatal("mainnet addresses missing")
	}
	base, err := order.Hash(ExchangeDomain(ChainIDBnbMainnet, mainnet, false))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	otherChain, err := order.Hash(ExchangeDomain(ChainIDBnbTestnet, mainnet, false))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	negRisk, err := order.Hash(ExchangeDomain(ChainIDBnbMainnet, mainnet, true))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if base == otherChain {
		t.Fatal("hash ignoring chainId: an order could be replayed on another chain")
	}
	if base == negRisk {
		t.Fatal("hash ignoring verifyingContract: a neg-risk order would verify on the plain exchange")
	}
	if mainnet.Exchange == mainnet.NegRiskExchange {
		t.Fatal("test fixture assumption broken: the two exchanges are the same address")
	}
}

func TestOrderTypedDataFields(t *testing.T) {
	key := testKey(t)
	order := fixedOrder(t, key)
	addresses, _ := AddressesFor(ChainIDBnbMainnet)
	typedData, err := order.TypedData(ExchangeDomain(ChainIDBnbMainnet, addresses, false))
	if err != nil {
		t.Fatalf("TypedData: %v", err)
	}
	fields := typedData.Types[orderType]
	expected := []struct{ name, kind string }{
		{"salt", "uint256"}, {"maker", "address"}, {"signer", "address"},
		{"taker", "address"}, {"tokenId", "uint256"}, {"makerAmount", "uint256"},
		{"takerAmount", "uint256"}, {"expiration", "uint256"}, {"nonce", "uint256"},
		{"feeRateBps", "uint256"}, {"side", "uint8"}, {"signatureType", "uint8"},
	}
	if len(fields) != len(expected) {
		t.Fatalf("order has %d fields, want %d", len(fields), len(expected))
	}
	for index, want := range expected {
		if fields[index].Name != want.name || fields[index].Type != want.kind {
			t.Fatalf("field %d = %s %s, want %s %s", index, fields[index].Type, fields[index].Name, want.kind, want.name)
		}
	}
	if typedData.Domain.Name != ProtocolName || typedData.Domain.Version != ProtocolVersion {
		t.Fatalf("domain name/version = %s/%s", typedData.Domain.Name, typedData.Domain.Version)
	}
}

func TestSignOrderRejectsInvalidInput(t *testing.T) {
	key := testKey(t)
	addresses, _ := AddressesFor(ChainIDBnbMainnet)
	domain := ExchangeDomain(ChainIDBnbMainnet, addresses, false)

	cases := map[string]func(*Order){
		"bad side":         func(order *Order) { order.Side = Side(2) },
		"smart wallet sig": func(order *Order) { order.SignatureType = SignatureType(2) },
		"zero token":       func(order *Order) { order.TokenID = nil },
		"salt too large":   func(order *Order) { order.Salt = big.NewInt(MaxSalt) },
		"negative amount":  func(order *Order) { order.MakerAmount = big.NewInt(-1) },
	}
	for name, mutate := range cases {
		order := fixedOrder(t, key)
		mutate(&order)
		if _, _, err := order.Sign(key, domain); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestPersonalSignRecoversSigner(t *testing.T) {
	key := testKey(t)
	message := "predict.fun login: 12345"
	signature, err := PersonalSign(key, message)
	if err != nil {
		t.Fatalf("PersonalSign: %v", err)
	}
	raw, err := hexutil.Decode(signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if raw[64] != 27 && raw[64] != 28 {
		t.Fatalf("signature v = %d, want 27 or 28", raw[64])
	}
	raw[64] -= 27
	publicKey, err := crypto.Ecrecover(accounts.TextHash([]byte(message)), raw)
	if err != nil {
		t.Fatalf("Ecrecover: %v", err)
	}
	recovered, err := crypto.UnmarshalPubkey(publicKey)
	if err != nil {
		t.Fatalf("UnmarshalPubkey: %v", err)
	}
	if got, want := crypto.PubkeyToAddress(*recovered), crypto.PubkeyToAddress(key.PublicKey); got != want {
		t.Fatalf("recovered signer = %s, want %s", got, want)
	}
}

func TestPersonalSignDoesNotUseTypedDataPrefix(t *testing.T) {
	key := testKey(t)
	message := "sign me"
	signature, err := PersonalSign(key, message)
	if err != nil {
		t.Fatalf("PersonalSign: %v", err)
	}
	raw, err := hexutil.Decode(signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	raw[64] -= 27
	// An EIP-712 digest of the same text must not recover the signer, which is
	// what proves the EIP-191 prefix is actually applied.
	typed := crypto.Keccak256([]byte(message))
	if publicKey, err := crypto.Ecrecover(typed, raw); err == nil {
		recovered, _ := crypto.UnmarshalPubkey(publicKey)
		if crypto.PubkeyToAddress(*recovered) == crypto.PubkeyToAddress(key.PublicKey) {
			t.Fatal("signature verifies without the EIP-191 prefix")
		}
	}
}

func TestRandomSaltStaysInRange(t *testing.T) {
	limit := big.NewInt(MaxSalt)
	for i := 0; i < 100; i++ {
		salt, err := RandomSalt()
		if err != nil {
			t.Fatalf("RandomSalt: %v", err)
		}
		if salt.Sign() < 0 || salt.Cmp(limit) >= 0 {
			t.Fatalf("salt %s outside [0,%d)", salt, MaxSalt)
		}
	}
}

func TestWireOrderSerializesAmountsAsDecimalStrings(t *testing.T) {
	key := testKey(t)
	order := fixedOrder(t, key)
	wire := order.signedOrder("0xsig")
	if wire.MakerAmount != order.MakerAmount.String() {
		t.Fatalf("makerAmount = %s, want %s", wire.MakerAmount, order.MakerAmount)
	}
	if wire.TokenID != order.TokenID.String() || strings.Contains(wire.TokenID, "e") {
		t.Fatalf("tokenId = %q", wire.TokenID)
	}
	if wire.Signature != "0xsig" || wire.Side != uint8(SideBuy) {
		t.Fatalf("wire order = %+v", wire)
	}
}
