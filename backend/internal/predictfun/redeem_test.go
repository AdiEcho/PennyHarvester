package predictfun

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestRedeemSelectors pins the two redemption entry points. They were derived
// from their signatures, and this test exists so a signature typo cannot
// silently produce a call to a different function: a wrong selector on a redeem
// either reverts (costing gas) or, worse, matches an unrelated function.
func TestRedeemSelectors(t *testing.T) {
	cases := []struct {
		name     string
		selector [4]byte
		want     string
	}{
		{name: "ctf", selector: selectorRedeemCTF, want: "01b7037c"},
		{name: "neg-risk", selector: selectorRedeemNegRisk, want: "dbeccb23"},
	}
	for _, tc := range cases {
		if got := hex.EncodeToString(tc.selector[:]); got != tc.want {
			t.Errorf("%s selector = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestEncodeRedeemPositionsCTF checks the plain-market calldata word by word.
// The layout is fixed by the ABI, so a mismatch is a bug, not a preference.
func TestEncodeRedeemPositionsCTF(t *testing.T) {
	collateral := common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
	conditionID := common.HexToHash("0xfbc814b46d9ebb059860c87111255d0df03a84d2412cd4b41eb4aaea5a722983")

	data := encodeRedeemPositionsCTF(collateral, conditionID, 2)
	if len(data) != 4+32*6 {
		t.Fatalf("calldata length = %d, want %d", len(data), 4+32*6)
	}
	if got := hex.EncodeToString(data[:4]); got != "01b7037c" {
		t.Errorf("selector = %s", got)
	}
	words := make([]string, 0, 6)
	for offset := 4; offset < len(data); offset += 32 {
		words = append(words, hex.EncodeToString(data[offset:offset+32]))
	}
	// Word 0: collateral, left-padded to 32 bytes.
	if want := strings.Repeat("0", 24) + "55d398326f99059ff775485246999027b3197955"; words[0] != want {
		t.Errorf("collateral word = %s, want %s", words[0], want)
	}
	// Word 1: parentCollectionId is the zero hash for a top-level condition.
	if want := strings.Repeat("0", 64); words[1] != want {
		t.Errorf("parentCollectionId word = %s, want zero", words[1])
	}
	if want := strings.TrimPrefix(conditionID.Hex(), "0x"); words[2] != want {
		t.Errorf("conditionId word = %s, want %s", words[2], want)
	}
	// Word 3: offset of the dynamic array, measured from the head start.
	if want := "0000000000000000000000000000000000000000000000000000000000000080"; words[3] != want {
		t.Errorf("array offset = %s, want %s", words[3], want)
	}
	if want := "0000000000000000000000000000000000000000000000000000000000000001"; words[4] != want {
		t.Errorf("array length = %s, want 1", words[4])
	}
	if want := "0000000000000000000000000000000000000000000000000000000000000002"; words[5] != want {
		t.Errorf("indexSets[0] = %s, want 2", words[5])
	}
}

// TestEncodeNegRiskRedeemPutsAmountInTheBurnedSlot checks the neg-risk calldata
// word by word. The point is the value's position: amounts is per outcome slot,
// so a Yes redemption must carry [shares, 0] and a No redemption [0, shares].
func TestEncodeNegRiskRedeemPutsAmountInTheBurnedSlot(t *testing.T) {
	conditionID := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	shares := new(big.Int).Mul(big.NewInt(3), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	word := func(value *big.Int) string {
		return hex.EncodeToString(padUint(value))
	}

	for _, tc := range []struct {
		indexSet int
		want     [2]string
	}{
		{indexSet: 1, want: [2]string{word(shares), word(nil)}},
		{indexSet: 2, want: [2]string{word(nil), word(shares)}},
	} {
		plan, err := planRedeem(common.Address{}, common.Address{}, common.HexToAddress("0xc3Cf7c252f65E0d8D88537dF96569AE94a7F1A6E"), true, conditionID, tc.indexSet, shares)
		if err != nil {
			t.Fatalf("indexSet %d: %v", tc.indexSet, err)
		}
		if plan.method != RedeemMethodNegRisk {
			t.Errorf("indexSet %d: method = %q", tc.indexSet, plan.method)
		}
		if got, want := plan.to, common.HexToAddress("0xc3Cf7c252f65E0d8D88537dF96569AE94a7F1A6E"); got != want {
			t.Errorf("indexSet %d: to = %s, want the neg-risk adapter %s", tc.indexSet, got, want)
		}
		// selector + conditionId + offset + length + two amounts = 6 words.
		if len(plan.data) != 4+32*5 {
			t.Fatalf("indexSet %d: calldata length = %d", tc.indexSet, len(plan.data))
		}
		words := make([]string, 0, 5)
		for offset := 4; offset < len(plan.data); offset += 32 {
			words = append(words, hex.EncodeToString(plan.data[offset:offset+32]))
		}
		if want := strings.TrimPrefix(conditionID.Hex(), "0x"); words[0] != want {
			t.Errorf("indexSet %d: conditionId = %s, want %s", tc.indexSet, words[0], want)
		}
		if want := word(big.NewInt(0x40)); words[1] != want {
			t.Errorf("indexSet %d: array offset = %s, want %s", tc.indexSet, words[1], want)
		}
		if want := word(big.NewInt(2)); words[2] != want {
			t.Errorf("indexSet %d: length = %s, want 2", tc.indexSet, words[2])
		}
		if words[3] != tc.want[0] || words[4] != tc.want[1] {
			t.Errorf("indexSet %d: amounts = [%s, %s], want [%s, %s]",
				tc.indexSet, words[3], words[4], tc.want[0], tc.want[1])
		}
	}
}

// TestPlanRedeemRejectsBadInput covers the inputs that would otherwise cost gas
// to be rejected on chain.
func TestPlanRedeemRejectsBadInput(t *testing.T) {
	conditionID := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	cases := []struct {
		name     string
		indexSet int
		shares   *big.Int
	}{
		{name: "zero shares", indexSet: 1, shares: new(big.Int)},
		{name: "nil shares", indexSet: 1, shares: nil},
		{name: "negative shares", indexSet: 1, shares: big.NewInt(-1)},
		{name: "index zero", indexSet: 0, shares: big.NewInt(1)},
		{name: "index three", indexSet: 3, shares: big.NewInt(1)},
	}
	for _, tc := range cases {
		if _, err := planRedeem(common.Address{}, common.Address{}, common.Address{}, false, conditionID, tc.indexSet, tc.shares); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// TestParseConditionIDRejectsShortIDs guards the encoding boundary: a truncated
// id would redeem nothing and still spend gas.
func TestParseConditionIDRejectsShortIDs(t *testing.T) {
	if _, err := parseConditionID("0xfbc814b4"); err == nil {
		t.Error("expected a short condition id to be rejected")
	}
	if _, err := parseConditionID(""); err == nil {
		t.Error("expected an empty condition id to be rejected")
	}
	hash, err := parseConditionID("0x000000000000000000000000000000000000000000000000000000000000000a")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hash.Hex() != "0x000000000000000000000000000000000000000000000000000000000000000a" {
		t.Errorf("hash = %s", hash.Hex())
	}
}

// TestIndexSetForRejectsForeignOutcomes keeps a position from being redeemed
// against the wrong market's condition.
func TestIndexSetForRejectsForeignOutcomes(t *testing.T) {
	meta := marketMeta{MarketID: "437", YesTokenID: "77", NoTokenID: "78"}
	if got, err := indexSetFor(meta, "77"); err != nil || got != 1 {
		t.Errorf("yes: got (%d, %v)", got, err)
	}
	if got, err := indexSetFor(meta, "78"); err != nil || got != 2 {
		t.Errorf("no: got (%d, %v)", got, err)
	}
	if _, err := indexSetFor(meta, "79"); err == nil {
		t.Error("expected a foreign token id to be rejected")
	}
	if _, err := indexSetFor(meta, ""); err == nil {
		t.Error("expected an empty token id to be rejected")
	}
}
