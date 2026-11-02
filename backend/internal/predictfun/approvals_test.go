package predictfun

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestSelectors(t *testing.T) {
	cases := []struct {
		name     string
		selector [4]byte
		want     string
	}{
		{name: "approve", selector: selectorApprove, want: "095ea7b3"},
		{name: "setApprovalForAll", selector: selectorSetApprovalForAll, want: "a22cb465"},
		{name: "allowance", selector: selectorAllowance, want: "dd62ed3e"},
		{name: "balanceOf", selector: selectorBalanceOf, want: "70a08231"},
		{name: "isApprovedForAll", selector: selectorApprovedForAll, want: "e985e9c5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hex.EncodeToString(tc.selector[:]); got != tc.want {
				t.Fatalf("selector = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestEncodeCalls(t *testing.T) {
	owner := common.HexToAddress("0x1111111111111111111111111111111111111111")
	operator := common.HexToAddress("0x2222222222222222222222222222222222222222")

	cases := []struct {
		name   string
		encode func() ([]byte, error)
		want   string
	}{
		{
			name: "approve max uint256",
			encode: func() ([]byte, error) {
				return encodeApprove(owner, maxUint256)
			},
			want: "0x095ea7b3" +
				"0000000000000000000000001111111111111111111111111111111111111111" +
				"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
		{
			name: "approve small amount",
			encode: func() ([]byte, error) {
				return encodeApprove(owner, big.NewInt(42))
			},
			want: "0x095ea7b3" +
				"0000000000000000000000001111111111111111111111111111111111111111" +
				"000000000000000000000000000000000000000000000000000000000000002a",
		},
		{
			name: "set approval for all true",
			encode: func() ([]byte, error) {
				return encodeSetApprovalForAll(operator, true)
			},
			want: "0xa22cb465" +
				"0000000000000000000000002222222222222222222222222222222222222222" +
				"0000000000000000000000000000000000000000000000000000000000000001",
		},
		{
			name: "allowance",
			encode: func() ([]byte, error) {
				return encodeAllowanceCall(owner, operator)
			},
			want: "0xdd62ed3e" +
				"0000000000000000000000001111111111111111111111111111111111111111" +
				"0000000000000000000000002222222222222222222222222222222222222222",
		},
		{
			name: "balance of",
			encode: func() ([]byte, error) {
				return encodeBalanceOf(owner)
			},
			want: "0x70a08231" +
				"0000000000000000000000001111111111111111111111111111111111111111",
		},
		{
			name: "approved for all",
			encode: func() ([]byte, error) {
				return encodeApprovedForAll(owner, operator)
			},
			want: "0xe985e9c5" +
				"0000000000000000000000001111111111111111111111111111111111111111" +
				"0000000000000000000000002222222222222222222222222222222222222222",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := tc.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := "0x" + hex.EncodeToString(encoded); got != tc.want {
				t.Fatalf("encoded call = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestApprovalStateAllowanceAndReady(t *testing.T) {
	one := big.NewInt(1_000_000_000_000_000_000)
	two := big.NewInt(2_000_000_000_000_000_000)

	allowanceCases := []struct {
		name  string
		state ApprovalState
		want  *big.Int
	}{
		{
			name:  "exchange only",
			state: ApprovalState{ExchangeAllowance: one},
			want:  one,
		},
		{
			name:  "neg-risk is larger",
			state: ApprovalState{ExchangeAllowance: one, NegRiskAllowance: two},
			want:  two,
		},
		{
			name:  "exchange is larger",
			state: ApprovalState{ExchangeAllowance: two, NegRiskAllowance: one},
			want:  two,
		},
		{
			name:  "both nil",
			state: ApprovalState{},
			want:  new(big.Int),
		},
		{
			name:  "one nil",
			state: ApprovalState{NegRiskAllowance: two},
			want:  two,
		},
	}
	for _, tc := range allowanceCases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.state.CollateralAllowance()
			if got == nil {
				t.Fatal("CollateralAllowance returned nil")
			}
			if got.Cmp(tc.want) != 0 {
				t.Fatalf("CollateralAllowance = %s, want %s", got, tc.want)
			}
		})
	}

	readyCases := []struct {
		name  string
		state ApprovalState
		min   *big.Int
		want  bool
	}{
		{
			name: "both approvals and enough allowance",
			state: ApprovalState{
				CollateralBalance: one,
				ExchangeAllowance: one,
				NegRiskAllowance:  one,
				ExchangeApproved:  true,
				NegRiskApproved:   true,
			},
			min:  one,
			want: true,
		},
		{
			name: "allowance below minimum",
			state: ApprovalState{
				ExchangeAllowance: one,
				NegRiskAllowance:  one,
				ExchangeApproved:  true,
				NegRiskApproved:   true,
			},
			min:  two,
			want: false,
		},
		{
			name: "neg-risk operator missing",
			state: ApprovalState{
				ExchangeAllowance: two,
				ExchangeApproved:  true,
			},
			min:  one,
			want: false,
		},
		{
			name: "nil minimum only checks shares",
			state: ApprovalState{
				ExchangeApproved: true,
				NegRiskApproved:  true,
			},
			min:  nil,
			want: true,
		},
		{
			name: "nil minimum still needs both operators",
			state: ApprovalState{
				ExchangeApproved: true,
			},
			min:  nil,
			want: false,
		},
	}
	for _, tc := range readyCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Ready(tc.min); got != tc.want {
				t.Fatalf("Ready(%v) = %v, want %v", tc.min, got, tc.want)
			}
		})
	}
}
