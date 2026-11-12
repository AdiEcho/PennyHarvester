package predictfun

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"pennyharvester/backend/internal/venue"
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

func TestSendCallPreservesHashWhenBroadcastIsUncertain(t *testing.T) {
	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e8a5")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// EstimateGas falls back, then the broadcast fails. A node can accept
		// the raw transaction and still lose the connection before answering.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"connection reset"}}`))
	}))
	defer server.Close()
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	chain := &OnChain{settings: settings{
		key:     key,
		signer:  crypto.PubkeyToAddress(key.PublicKey),
		chainID: 97,
	}}
	_, err = chain.sendCall(context.Background(), client, 7, big.NewInt(1), approvalCall{
		to:     common.HexToAddress("0x0000000000000000000000000000000000000001"),
		data:   []byte{0x01},
		action: "测试授权",
	}, nil)
	if err == nil {
		t.Fatal("expected broadcast error")
	}
	hash, ok := TxHashOf(err)
	if !ok || hash == (common.Hash{}) {
		t.Fatalf("broadcast error lost the signed hash: %v", err)
	}
	if !strings.Contains(err.Error(), hash.Hex()) {
		t.Fatalf("error text %q does not identify transaction %s", err, hash.Hex())
	}
}

func TestSendCallJournalsSignedTransactionBeforeBroadcast(t *testing.T) {
	key := testKey(t)
	var mu sync.Mutex
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "eth_estimateGas":
			// Force the adapter onto its fixed gas fallback.
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"estimate unavailable"}}`, call.ID)
		case "eth_sendRawTransaction":
			mu.Lock()
			events = append(events, "send")
			mu.Unlock()
			// The node can accept the raw transaction and still lose the
			// connection before answering, so the caller must keep the hash.
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"connection reset"}}`, call.ID)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unexpected call"}}`, call.ID)
		}
	}))
	defer server.Close()
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	chain := &OnChain{settings: settings{
		key:     key,
		signer:  crypto.PubkeyToAddress(key.PublicKey),
		chainID: 97,
	}}
	var signed venue.ChainTxSigned
	_, err = chain.sendCall(context.Background(), client, 7, big.NewInt(1), approvalCall{
		to:     common.HexToAddress("0x0000000000000000000000000000000000000001"),
		data:   []byte{0x01},
		action: "测试授权",
	}, func(tx venue.ChainTxSigned) error {
		mu.Lock()
		events = append(events, "signed")
		mu.Unlock()
		signed = tx
		return nil
	})
	if err == nil {
		t.Fatal("expected broadcast error")
	}
	mu.Lock()
	order := append([]string(nil), events...)
	mu.Unlock()
	if len(order) != 2 || order[0] != "signed" || order[1] != "send" {
		t.Fatalf("signature must be journaled before broadcast, got %v", order)
	}
	if signed.Hash == "" || signed.From != chain.settings.signer.Hex() || signed.Nonce != 7 || signed.ChainID != 97 {
		t.Fatalf("signed identity = %+v", signed)
	}
	hash, ok := TxHashOf(err)
	if !ok || hash.Hex() != signed.Hash {
		t.Fatalf("broadcast error hash = %s (ok=%v), signed %s", hash.Hex(), ok, signed.Hash)
	}
}

func TestSendCallDoesNotBroadcastWhenJournalCallbackFails(t *testing.T) {
	key := testKey(t)
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "eth_estimateGas":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"estimate unavailable"}}`, call.ID)
		case "eth_sendRawTransaction":
			sent = true
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x00"}`, call.ID)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unexpected call"}}`, call.ID)
		}
	}))
	defer server.Close()
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	chain := &OnChain{settings: settings{
		key:     key,
		signer:  crypto.PubkeyToAddress(key.PublicKey),
		chainID: 97,
	}}
	_, err = chain.sendCall(context.Background(), client, 8, big.NewInt(1), approvalCall{
		to:     common.HexToAddress("0x0000000000000000000000000000000000000001"),
		data:   []byte{0x01},
		action: "测试授权",
	}, func(venue.ChainTxSigned) error {
		return errors.New("journal unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "未广播") {
		t.Fatalf("journal failure must stop the send, got %v", err)
	}
	if sent {
		t.Fatal("transaction was broadcast after the journal callback failed")
	}
	if _, ok := TxHashOf(err); ok {
		t.Fatalf("a transaction that was never sent must not report a hash: %v", err)
	}
}

func TestSendCallPreservesHashWhenReceiptTimesOut(t *testing.T) {
	key := testKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := false
	receiptCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "eth_estimateGas":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"estimate unavailable"}}`, call.ID)
		case "eth_sendRawTransaction":
			sent = true
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x%s"}`, call.ID, strings.Repeat("1", 64))
		case "eth_getTransactionReceipt":
			receiptCalls++
			if receiptCalls >= 3 {
				cancel()
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":null}`, call.ID)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unexpected call"}}`, call.ID)
		}
	}))
	defer server.Close()
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	chain := &OnChain{settings: settings{
		key:     key,
		signer:  crypto.PubkeyToAddress(key.PublicKey),
		chainID: 97,
	}, sleep: func(time.Duration) {}}
	_, err = chain.sendCall(ctx, client, 9, big.NewInt(1), approvalCall{
		to:     common.HexToAddress("0x0000000000000000000000000000000000000001"),
		data:   []byte{0x01},
		action: "测试授权",
	}, nil)
	if err == nil {
		t.Fatal("expected receipt timeout")
	}
	if !sent {
		t.Fatal("transaction was not broadcast")
	}
	var txErr *TxError
	if !errors.As(err, &txErr) || txErr.Phase != "receipt" || txErr.Reverted {
		t.Fatalf("receipt timeout error = %#v", err)
	}
	if txErr.Hash == (common.Hash{}) {
		t.Fatal("receipt timeout lost the signed hash")
	}
}

func TestOnChainRejectsRPCFromAnotherNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "eth_chainId":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x38"}`)) // 56
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"unexpected call"}}`))
		}
	}))
	defer server.Close()
	chain, err := NewOnChain(Config{
		PrivateKey: "4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e8a5",
		Testnet:    true, // chainId 97
		RPCURL:     server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Allowance(context.Background()); err == nil || !strings.Contains(err.Error(), "chainId") {
		t.Fatalf("mismatched network was not rejected: %v", err)
	}
}
