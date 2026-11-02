package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"errors"

	"pennyharvester/backend/internal/api"
	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/store"
	"pennyharvester/backend/internal/venue"
)

// approverLive is a venue executor that also reports on-chain allowances, the
// way the Predict.fun executor does.
type approverLive struct {
	quoteLive
	ready   bool
	granted int
}

func (a *approverLive) Configured() bool { return true }

func (a *approverLive) ApprovalState(context.Context) (venue.Approval, error) {
	return venue.Approval{
		Venue: market.VenuePredictFun, Collateral: "USDT", Balance: 12.5,
		Allowance: 0, SharesReady: a.ready, Ready: false, Missing: "尚未授权交易所扣除 USDT",
	}, nil
}

func (a *approverLive) GrantApprovals(context.Context) (venue.Approval, error) {
	a.granted++
	approval, _ := a.ApprovalState(context.Background())
	approval.Allowance = 12.5
	approval.SharesReady = true
	approval.Ready = true
	approval.Missing = ""
	return approval, nil
}

// newApprovalRouter registers both venues: Polymarket read-only (the shape the
// default deployment has) and Predict.fun with an executor that needs approvals.
func newApprovalRouter(t *testing.T) (http.Handler, *approverLive) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repository, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	live := &approverLive{}
	registry := apiTestRegistry(t, testProvider(), nil)
	if err := registry.Register(venue.Capabilities{
		Venue: market.VenuePredictFun, Label: "Predict.fun", Currency: market.CurrencyUSDT,
		QuoteDecimals: 18, OrderTypes: []string{"LIMIT", "MARKET"},
	}, pfStubProvider{}, live); err != nil {
		t.Fatal(err)
	}
	instance, err := engine.New(context.Background(), repository, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewRouter(instance, "", api.NewAuthenticator(repository)), live
}

func TestVenueApprovalReportsStateAndGrants(t *testing.T) {
	handler, live := newApprovalRouter(t)
	cookies := approvalSession(t, handler)

	response := requestWithCookie(t, handler, http.MethodGet, "/api/v1/system/live/approval?venue=predictfun", nil, cookies)
	if response.Code != http.StatusOK {
		t.Fatalf("read approval: %d %s", response.Code, response.Body.String())
	}
	var read struct {
		Venue    string         `json:"venue"`
		Approval venue.Approval `json:"approval"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.Approval.Ready || read.Approval.Allowance != 0 || read.Approval.Balance != 12.5 {
		t.Fatalf("read approval = %+v", read.Approval)
	}
	if read.Approval.Missing == "" {
		t.Fatal("a not-ready approval must explain what is missing")
	}

	response = requestWithCookie(t, handler, http.MethodPost, "/api/v1/system/live/approve?venue=predictfun", nil, cookies)
	if response.Code != http.StatusOK {
		t.Fatalf("grant approval: %d %s", response.Code, response.Body.String())
	}
	var granted struct {
		Approval venue.Approval `json:"approval"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &granted); err != nil {
		t.Fatal(err)
	}
	if !granted.Approval.Ready || granted.Approval.Allowance != 12.5 || live.granted != 1 {
		t.Fatalf("granted approval = %+v (calls %d)", granted.Approval, live.granted)
	}
}

func TestVenueApprovalRejectsUnknownAndReadOnlyVenues(t *testing.T) {
	handler, _ := newApprovalRouter(t)
	cookies := approvalSession(t, handler)

	cases := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{name: "unknown venue", method: http.MethodGet, path: "/api/v1/system/live/approval?venue=bogus", want: "未知场所"},
		// Polymarket is registered without an executor in this deployment, so it
		// can trade nothing and must say so instead of reporting an empty state.
		{name: "read-only venue", method: http.MethodGet, path: "/api/v1/system/live/approval?venue=polymarket", want: "只读"},
		{name: "approve unknown", method: http.MethodPost, path: "/api/v1/system/live/approve?venue=bogus", want: "未知场所"},
		{name: "approve read-only", method: http.MethodPost, path: "/api/v1/system/live/approve?venue=polymarket", want: "只读"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := requestWithCookie(t, handler, testCase.method, testCase.path, nil, cookies)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", response.Code, response.Body.String())
			}
			if body := response.Body.String(); !strings.Contains(body, testCase.want) {
				t.Fatalf("body = %s, want it to mention %q", body, testCase.want)
			}
		})
	}
}

// TestVenueApprovalRejectsProxyTradedVenue covers "nothing to approve": a venue
// whose executor is not an Approver must answer with a client error, not a
// silent success that would make the UI show an approve button forever.
// readOnlyApprovalLive is a Polymarket-shaped executor: it can trade but has no
// on-chain approval step of its own.
type readOnlyApprovalLive struct {
	quoteLive
}

func TestVenueApprovalRejectsProxyTradedVenue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	registry := apiTestRegistry(t, testProvider(), &readOnlyApprovalLive{})
	instance, err := engine.New(context.Background(), repository, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NewRouter(instance, "", api.NewAuthenticator(repository))
	cookies := approvalSession(t, handler)
	response := requestWithCookie(t, handler, http.MethodGet, "/api/v1/system/live/approval?venue=polymarket", nil, cookies)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "无需链上授权") {
		t.Fatalf("body = %s", body)
	}
}

// pfStubProvider is an empty Predict.fun market data source: the approval tests
// only exercise the executor side, and the registry refuses a venue without one.
type pfStubProvider struct{}

func (pfStubProvider) Venue() market.Venue { return market.VenuePredictFun }
func (pfStubProvider) Name() string        { return "predictfun-stub" }
func (pfStubProvider) ListOpen(context.Context, int) ([]market.Market, error) {
	return nil, nil
}
func (pfStubProvider) Get(context.Context, string) (market.Market, error) {
	return market.Market{}, errors.New("no markets")
}

// approvalSession logs in with a password this test set, so the fixtures do not
// depend on the literal every other auth test uses.
func approvalSession(t *testing.T, handler http.Handler) []*http.Cookie {
	t.Helper()
	const password = "approval-test-password"
	if response := request(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "", "new_password": password,
	}); response.Code != http.StatusNoContent {
		t.Fatalf("setting the password failed: %d %s", response.Code, response.Body.String())
	}
	login := request(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{"password": password})
	if login.Code != http.StatusNoContent {
		t.Fatalf("login failed: %d %s", login.Code, login.Body.String())
	}
	return sessionCookies(t, login)
}
