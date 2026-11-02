package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
)

// proxyProbeURL is a fixed, cheap Polymarket endpoint so the connectivity test
// cannot be turned into a request against an arbitrary host from the browser.
const proxyProbeURL = "https://clob.polymarket.com/time"

func (h *Handler) systemSettings(c *gin.Context) {
	h.writeSystemSettings(c)
}

func (h *Handler) updateSystemSettings(c *gin.Context) {
	var request struct {
		Proxy      *netproxy.Config                            `json:"proxy"`
		DataSource *engine.DataSourceSettings                  `json:"data_source"`
		Live       *engine.LiveSettings                        `json:"live"`
		Venues     map[market.Venue]engine.VenueSystemSettings `json:"venues"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "系统设置 JSON 无效: " + err.Error()})
		return
	}
	// Treat the payload as a partial update so saving one section cannot wipe
	// the other one. Masked secrets echoed back by the browser are restored
	// from the stored values.
	current := h.engine.SystemSettings()
	settings := current
	if request.Proxy != nil {
		settings.Proxy = request.Proxy.MergeSecret(current.Proxy)
	}
	if request.DataSource != nil {
		settings.DataSource = *request.DataSource
	}
	if request.Live != nil {
		settings.Live = request.Live.MergeSecret(current.Live)
	}
	if request.Venues != nil {
		venues := make(map[market.Venue]engine.VenueSystemSettings, len(current.Venues)+len(request.Venues))
		for v, entry := range current.Venues {
			venues[v] = entry
		}
		for v, entry := range request.Venues {
			if !v.Valid() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "未知场所"})
				return
			}
			venues[v] = entry.Merge(current.Venues[v])
		}
		settings.Venues = venues
	}
	// Dual read/write for Polymarket: when the payload carries venues.polymarket
	// it wins over the flat fields; otherwise the flat fields win. Either way
	// both views are written so an older frontend keeps working.
	if pm, ok := request.Venues[market.VenuePolymarket]; ok {
		merged := pm.Merge(current.Venues[market.VenuePolymarket])
		settings.DataSource = engine.DataSourceSettings{
			GammaBaseURL: merged.DataSource.GammaBaseURL,
			CLOBBaseURL:  merged.DataSource.CLOBBaseURL,
		}
		settings.Live = merged.Live.LiveSettings
	}
	settings = settings.SyncPolymarketVenue()
	if err := settings.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.engine.UpdateSystemSettings(c.Request.Context(), settings); err != nil {
		respondError(c, err)
		return
	}
	h.writeSystemSettings(c)
}

func (h *Handler) writeSystemSettings(c *gin.Context) {
	settings := h.engine.SystemSettings()
	enabled, err := h.auth.Enabled(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"proxy":           settings.Proxy.Redacted(),
		"data_source":     settings.DataSource,
		"live":            settings.Live.Redacted(),
		"venues":          venueSettingsView(settings),
		"runtime":         h.engine.SystemRuntime(),
		"password_set":    enabled,
		"proxy_probe_url": proxyProbeURL,
		"environment": gin.H{
			"http_proxy_set":  envPresent("HTTP_PROXY"),
			"https_proxy_set": envPresent("HTTPS_PROXY"),
			"no_proxy":        envValue("NO_PROXY"),
		},
	})
}

// venueSettingsView is the API projection of the per-venue settings: secrets
// are redacted and each one gets a *_configured boolean so the UI can show
// "已保存" without receiving the value.
type venueSettingsViewEntry struct {
	DataSource            engine.VenueDataSourceSettings `json:"data_source"`
	Live                  engine.VenueLiveSettings       `json:"live"`
	DataSourceConfigured  bool                           `json:"data_source_configured"`
	LiveConfigured        bool                           `json:"live_configured"`
	PrivateKeyConfigured  bool                           `json:"private_key_configured"`
	APIKeyConfigured      bool                           `json:"api_key_configured"`
	SmartWalletConfigured bool                           `json:"smart_wallet_configured"`
}

func venueSettingsView(settings engine.SystemSettings) map[market.Venue]venueSettingsViewEntry {
	view := make(map[market.Venue]venueSettingsViewEntry, len(settings.Venues))
	for v, entry := range settings.Venues {
		redacted := entry.Redacted()
		view[v] = venueSettingsViewEntry{
			DataSource:            redacted.DataSource,
			Live:                  redacted.Live,
			DataSourceConfigured:  entry.DataSource.GammaBaseURL != "" || entry.DataSource.CLOBBaseURL != "" || entry.DataSource.BaseURL != "",
			LiveConfigured:        entry.Live.PrivateKey != "" || entry.Live.APIKey != "",
			PrivateKeyConfigured:  entry.Live.PrivateKey != "",
			APIKeyConfigured:      entry.Live.APIKey != "",
			SmartWalletConfigured: entry.Live.SmartWallet != "",
		}
	}
	return view
}

// testProxy dials the probe endpoint through the proxy configuration in the
// request body (falling back to the stored one) without persisting anything.
func (h *Handler) testProxy(c *gin.Context) {
	var request struct {
		Proxy netproxy.Config `json:"proxy"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "代理测试请求无效: " + err.Error()})
		return
	}
	config := request.Proxy.MergeSecret(h.engine.SystemSettings().Proxy)
	manager, err := netproxy.New(config)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyProbeURL, nil)
	if err != nil {
		respondError(c, err)
		return
	}
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	response, err := manager.HTTPClient(12 * time.Second).Do(req)
	latency := float64(time.Since(start).Microseconds()) / 1000
	result := gin.H{
		"target":     proxyProbeURL,
		"mode":       config.Normalize().Mode,
		"latency_ms": latency,
	}
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		c.JSON(http.StatusOK, result)
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	result["ok"] = response.StatusCode >= 200 && response.StatusCode < 400
	result["status_code"] = response.StatusCode
	if !result["ok"].(bool) {
		result["error"] = "上游返回 " + response.Status
	}
	c.JSON(http.StatusOK, result)
}

// testLive validates the live credentials in the request body (falling back to
// the stored values) against the CLOB. It derives the L2 credentials and reads
// the collateral balance, so it proves the wallet, funder and signature type
// agree without placing an order or persisting anything.
func (h *Handler) testLive(c *gin.Context) {
	v, ok := venueFromQuery(c)
	if !ok {
		return
	}
	var request struct {
		Live        engine.VenueLiveSettings `json:"live"`
		CLOBBaseURL string                   `json:"clob_base_url"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实盘凭据测试请求无效: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	probe, err := h.engine.ProbeLiveVenue(ctx, v, request.Live, request.CLOBBaseURL)
	if err != nil {
		status := http.StatusOK
		if errors.Is(err, engine.ErrInvalidSettings) || errors.Is(err, engine.ErrVenueReadOnly) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"ok": false, "venue": effectiveVenueLabel(v), "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"venue":          effectiveVenueLabel(v),
		"signer":         probe.Signer,
		"maker":          probe.Maker,
		"api_key":        probe.APIKey,
		"source":         probe.Source,
		"mode":           probe.Mode,
		"signature_type": probe.SignatureType,
		"cash":           probe.Cash,
		"allowance":      probe.Allowance,
		"latency_ms":     probe.LatencyMS,
	})
}

// venueApproval reports the on-chain allowances a venue needs before it can
// trade. It is read-only: no transaction is sent.
func (h *Handler) venueApproval(c *gin.Context) {
	v, ok := venueFromQuery(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	approval, err := h.engine.VenueApproval(ctx, v)
	if err != nil {
		c.JSON(approvalErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"venue": effectiveVenueLabel(v), "approval": approval})
}

// approveVenue sends the missing approval transactions. The wallet pays gas, so
// this is an explicit user action and never happens during a scan.
func (h *Handler) approveVenue(c *gin.Context) {
	v, ok := venueFromQuery(c)
	if !ok {
		return
	}
	// An approval is a real transaction on a public chain; give it room for two
	// confirmations instead of the read-only endpoints' timeout.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	approval, err := h.engine.ApproveVenue(ctx, v)
	if err != nil {
		c.JSON(approvalErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"venue": effectiveVenueLabel(v), "approval": approval})
}

// approvalErrorStatus maps the engine's sentinel errors onto HTTP statuses: a
// read-only venue or a proxy-traded one is a client error, everything else came
// from the chain.
// redeemPosition burns a settled position's shares back into collateral. It is
// the only endpoint that sends a transaction whose purpose is to receive money,
// so it waits for the receipt inside the request instead of answering early.
func (h *Handler) redeemPosition(c *gin.Context) {
	accountID, ok := parseID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户 ID 无效"})
		return
	}
	positionID, ok := parseID(c, "positionId")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仓位 ID 无效"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	result, err := h.engine.RedeemPosition(ctx, accountID, positionID)
	if err != nil {
		c.JSON(redeemErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"venue":   effectiveVenueLabel(result.Venue),
		"tx_hash": result.TxHash,
		"method":  result.Method,
	})
}

// redeemErrorStatus separates "this cannot work" (400/409) from "the chain or
// the RPC failed" (502), so a retry decision can be made from the status alone.
func redeemErrorStatus(err error) int {
	switch {
	case errors.Is(err, engine.ErrAccountNotFound):
		return http.StatusNotFound
	case errors.Is(err, engine.ErrInvalidOrder), errors.Is(err, engine.ErrNotFound):
		return http.StatusBadRequest
	case errors.Is(err, engine.ErrVenueReadOnly), errors.Is(err, engine.ErrNoApproval),
		errors.Is(err, engine.ErrVenueNotConfigured), errors.Is(err, engine.ErrInvalidSettings):
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}

func approvalErrorStatus(err error) int {
	switch {
	case errors.Is(err, engine.ErrVenueReadOnly), errors.Is(err, engine.ErrNoApproval),
		errors.Is(err, engine.ErrVenueNotConfigured), errors.Is(err, engine.ErrInvalidSettings):
		// Same mapping the rest of the API uses for these sentinels, so a client
		// does not have to special-case the approval endpoints.
		if errors.Is(err, engine.ErrVenueNotConfigured) {
			return http.StatusConflict
		}
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// testRelayer verifies a Relayer API key against the relayer's key list. It
// does not save the key and cannot place an order: relaying only broadcasts
// wallet batches that the signer has already signed.
func (h *Handler) testRelayer(c *gin.Context) {
	v, ok := venueFromQuery(c)
	if !ok {
		return
	}
	var request struct {
		Live          engine.LiveSettings `json:"live"`
		CLOBBaseURL   string              `json:"clob_base_url"`
		RelayerAPIKey string              `json:"relayer_api_key"`
		RelayerAddr   string              `json:"relayer_address"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Relayer 凭据测试请求无效: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	probe, err := h.engine.ProbeRelayer(ctx, v, request.Live, request.CLOBBaseURL, request.RelayerAPIKey, request.RelayerAddr)
	if err != nil {
		status := http.StatusOK
		if errors.Is(err, engine.ErrInvalidSettings) || errors.Is(err, engine.ErrVenueReadOnly) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"ok": false, "venue": effectiveVenueLabel(v), "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"venue":      effectiveVenueLabel(v),
		"signer":     probe.Signer,
		"address":    probe.Address,
		"api_key":    probe.APIKey,
		"key_count":  probe.KeyCount,
		"latency_ms": probe.LatencyMS,
	})
}

// discoverFunder resolves the Polymarket account wallet (funder) of the signer
// in the request body and detects its wallet type. It saves nothing, derives no
// L2 credentials and needs no funder, so the settings page can fill both fields
// for a wallet that was just connected to polymarket.com.
func (h *Handler) discoverFunder(c *gin.Context) {
	v, ok := venueFromQuery(c)
	if !ok {
		return
	}
	var request struct {
		Live        engine.LiveSettings `json:"live"`
		CLOBBaseURL string              `json:"clob_base_url"`
		GammaURL    string              `json:"gamma_base_url"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "钱包检测请求无效: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	discovery, err := h.engine.DiscoverWallet(ctx, v, request.Live, engine.WalletEndpoints{
		CLOB:  request.CLOBBaseURL,
		Gamma: request.GammaURL,
	})
	payload := gin.H{
		"ok":               err == nil,
		"venue":            effectiveVenueLabel(v),
		"signer":           discovery.Signer,
		"funder":           discovery.Funder,
		"wallet_type":      discovery.WalletType,
		"wallet_type_name": discovery.WalletName,
		"deployed":         discovery.Deployed,
		"source":           discovery.Source,
		"wallets":          discovery.Wallets,
		"candidates":       discovery.Candidates,
		"notes":            discovery.Notes,
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	c.JSON(http.StatusOK, payload)
}

// effectiveVenueLabel is only used for response metadata; an absent parameter
// means Polymarket per the API contract.
func effectiveVenueLabel(v market.Venue) market.Venue {
	if v == "" {
		return market.VenuePolymarket
	}
	return v
}

func envPresent(key string) bool {
	return envValue(key) != ""
}

func envValue(key string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(strings.ToLower(key)))
}
