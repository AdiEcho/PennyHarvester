package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"pennyharvester/backend/internal/api"
	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/store"

	"github.com/gin-gonic/gin"
)

func setupChainOperationRouter(t *testing.T) (http.Handler, *store.SQLite, int64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	repository, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	registry := apiTestRegistry(t, testProvider(), nil)
	instance, err := engine.New(ctx, repository, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := repository.ListAccounts(ctx)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("expected seeded accounts: %+v err=%v", accounts, err)
	}
	return api.NewRouter(instance, "", api.NewAuthenticator(repository)), repository, accounts[0].ID
}

func seedUnknownOperation(t *testing.T, repository *store.SQLite, accountID, positionID int64, hash string) engine.ChainOperation {
	t.Helper()
	ctx := context.Background()
	position := positionID
	operation, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: accountID, Venue: market.VenuePolymarket, ChainID: 137, Kind: "redeem",
		PositionID: &position, MarketID: "market", TokenID: "token",
		FromAddress: "0x0000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		if err := repository.SetChainOperationHash(ctx, operation.ID, hash, operation.FromAddress, nil, 137); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.FinishChainOperation(ctx, operation.ID, "unknown", "等待运营核对链上记录"); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repository.ChainOperationByID(ctx, operation.ID)
	if err != nil || !found {
		t.Fatalf("reload seeded operation: found=%v err=%v", found, err)
	}
	return stored
}

func decodeChainOperationPage(t *testing.T, raw []byte) engine.ChainOperationPage {
	t.Helper()
	var page engine.ChainOperationPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode chain operation page: %v (%s)", err, raw)
	}
	return page
}

func TestChainOperationListAPI(t *testing.T) {
	handler, repository, accountID := setupChainOperationRouter(t)
	unknown := seedUnknownOperation(t, repository, accountID, 101, "")
	resolved := seedUnknownOperation(t, repository, accountID, 102, "0x"+strings.Repeat("1", 64))
	if _, err := repository.ResolveChainOperation(context.Background(), resolved.ID, "confirmed", "链上成功", "ops", ""); err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/api/v1/chain-operations?account_id=%d&venue=polymarket&status=unknown&limit=10&offset=0", accountID)
	response := request(t, handler, http.MethodGet, path, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list chain operations: %d %s", response.Code, response.Body.String())
	}
	page := decodeChainOperationPage(t, response.Body.Bytes())
	if page.Total != 1 || page.Limit != 10 || page.Offset != 0 || len(page.Items) != 1 {
		t.Fatalf("unexpected unknown page: %+v", page)
	}
	if page.Items[0].ID != unknown.ID || page.Items[0].Status != "unknown" {
		t.Fatalf("list returned the wrong row: %+v", page.Items[0])
	}

	all := request(t, handler, http.MethodGet, "/api/v1/chain-operations?status=all", nil)
	if all.Code != http.StatusOK {
		t.Fatalf("list all chain operations: %d %s", all.Code, all.Body.String())
	}
	allPage := decodeChainOperationPage(t, all.Body.Bytes())
	if allPage.Total != 2 || allPage.Limit != 50 || len(allPage.Items) != 2 {
		t.Fatalf("unexpected all page: %+v", allPage)
	}
	if allPage.Items[0].ID != resolved.ID {
		t.Fatalf("rows must be ordered by newest update first, got %+v", allPage.Items)
	}
	zeroAccount := request(t, handler, http.MethodGet, "/api/v1/chain-operations?account_id=0&status=all", nil)
	if zeroAccount.Code != http.StatusOK {
		t.Fatalf("account_id=0 must mean all accounts: %d %s", zeroAccount.Code, zeroAccount.Body.String())
	}
	if zeroPage := decodeChainOperationPage(t, zeroAccount.Body.Bytes()); zeroPage.Total != 2 {
		t.Fatalf("account_id=0 leaked or dropped rows: %+v", zeroPage)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{"bad account", "/api/v1/chain-operations?account_id=abc"},
		{"negative account", "/api/v1/chain-operations?account_id=-1"},
		{"bad venue", "/api/v1/chain-operations?venue=nowhere"},
		{"bad status", "/api/v1/chain-operations?status=done"},
		{"zero limit", "/api/v1/chain-operations?limit=0"},
		{"oversized limit", "/api/v1/chain-operations?limit=201"},
		{"non-numeric limit", "/api/v1/chain-operations?limit=many"},
		{"negative offset", "/api/v1/chain-operations?offset=-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := request(t, handler, http.MethodGet, test.path, nil)
			if bad.Code != http.StatusBadRequest {
				t.Fatalf("%s should be 400, got %d %s", test.path, bad.Code, bad.Body.String())
			}
		})
	}
}

func TestChainOperationResolveAPI(t *testing.T) {
	handler, repository, accountID := setupChainOperationRouter(t)
	operation := seedUnknownOperation(t, repository, accountID, 201, "")
	hash := "0x" + strings.Repeat("2", 64)
	path := fmt.Sprintf("/api/v1/chain-operations/%d/resolve", operation.ID)
	body := map[string]string{
		"status": "confirmed", "reason": "运营核对链上回执成功", "actor": "ops@example.com", "tx_hash": hash,
	}
	response := request(t, handler, http.MethodPost, path, body)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve chain operation: %d %s", response.Code, response.Body.String())
	}
	var resolved engine.ChainOperation
	if err := json.Unmarshal(response.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.ID != operation.ID || resolved.Status != "confirmed" || resolved.ResolvedBy != "ops@example.com" ||
		resolved.ResolutionNote != "运营核对链上回执成功" || resolved.ResolvedAt == nil || resolved.TxHash != hash {
		t.Fatalf("resolve response lacks audit fields: %+v", resolved)
	}
	if resolved.Reason != "等待运营核对链上记录" {
		t.Fatalf("resolve must keep the original reason: %+v", resolved)
	}

	replay := request(t, handler, http.MethodPost, path, body)
	if replay.Code != http.StatusOK {
		t.Fatalf("repeating the same resolution must be idempotent: %d %s", replay.Code, replay.Body.String())
	}
	var replayed engine.ChainOperation
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Status != "confirmed" || !replayed.ResolvedAt.Equal(*resolved.ResolvedAt) {
		t.Fatalf("idempotent replay changed the row: %+v vs %+v", replayed, resolved)
	}

	conflict := request(t, handler, http.MethodPost, path, map[string]string{
		"status": "reverted", "reason": "另一个结果", "actor": "ops@example.com",
	})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different final outcome must be 409, got %d %s", conflict.Code, conflict.Body.String())
	}

	reverted := seedUnknownOperation(t, repository, accountID, 202, "0x"+strings.Repeat("3", 64))
	revertedPath := fmt.Sprintf("/api/v1/chain-operations/%d/resolve", reverted.ID)
	revertedResponse := request(t, handler, http.MethodPost, revertedPath, map[string]string{
		"status": "reverted", "reason": "链上已回滚", "actor": "ops",
	})
	if revertedResponse.Code != http.StatusOK {
		t.Fatalf("reverted resolution: %d %s", revertedResponse.Code, revertedResponse.Body.String())
	}
	var revertedOp engine.ChainOperation
	if err := json.Unmarshal(revertedResponse.Body.Bytes(), &revertedOp); err != nil {
		t.Fatal(err)
	}
	if revertedOp.Status != "reverted" || revertedOp.TxHash != reverted.TxHash {
		t.Fatalf("unexpected reverted operation: %+v", revertedOp)
	}

	withoutHash := seedUnknownOperation(t, repository, accountID, 203, "")
	withoutHashPath := fmt.Sprintf("/api/v1/chain-operations/%d/resolve", withoutHash.ID)
	for _, test := range []struct {
		name string
		path string
		body map[string]string
		want int
	}{
		{"missing id", "/api/v1/chain-operations/999999/resolve", body, http.StatusNotFound},
		{"malformed id", "/api/v1/chain-operations/abc/resolve", body, http.StatusBadRequest},
		{"zero id", "/api/v1/chain-operations/0/resolve", body, http.StatusBadRequest},
		{"invalid status", path, map[string]string{"status": "unknown", "reason": "r", "actor": "a"}, http.StatusBadRequest},
		{"missing actor", withoutHashPath, map[string]string{"status": "reverted", "reason": "r", "tx_hash": hash}, http.StatusBadRequest},
		{"missing reason", withoutHashPath, map[string]string{"status": "reverted", "actor": "a", "tx_hash": hash}, http.StatusBadRequest},
		{"short hash", withoutHashPath, map[string]string{"status": "reverted", "reason": "r", "actor": "a", "tx_hash": "0x12"}, http.StatusBadRequest},
		{"confirmed without hash", withoutHashPath, map[string]string{"status": "confirmed", "reason": "r", "actor": "a"}, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := request(t, handler, http.MethodPost, test.path, test.body)
			if bad.Code != test.want {
				t.Fatalf("%s should be %d, got %d %s", test.name, test.want, bad.Code, bad.Body.String())
			}
		})
	}

	position := int64(204)
	intent, err := repository.BeginChainOperation(context.Background(), engine.ChainOperation{
		AccountID: accountID, Venue: market.VenuePolymarket, ChainID: 137, Kind: "redeem",
		PositionID: &position, FromAddress: "0x0000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := request(t, handler, http.MethodPost, fmt.Sprintf("/api/v1/chain-operations/%d/resolve", intent.ID), body)
	if active.Code != http.StatusConflict {
		t.Fatalf("non-unknown row must be 409, got %d %s", active.Code, active.Body.String())
	}
}
