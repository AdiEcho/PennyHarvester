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
		Proxy      *netproxy.Config           `json:"proxy"`
		DataSource *engine.DataSourceSettings `json:"data_source"`
		Live       *engine.LiveSettings       `json:"live"`
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
	var request struct {
		Live        engine.LiveSettings `json:"live"`
		CLOBBaseURL string              `json:"clob_base_url"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实盘凭据测试请求无效: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	probe, err := h.engine.ProbeLive(ctx, request.Live, request.CLOBBaseURL)
	if err != nil {
		status := http.StatusOK
		if errors.Is(err, engine.ErrInvalidSettings) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
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

// testRelayer verifies a Relayer API key against the relayer's key list. It
// does not save the key and cannot place an order: relaying only broadcasts
// wallet batches that the signer has already signed.
func (h *Handler) testRelayer(c *gin.Context) {
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
	probe, err := h.engine.ProbeRelayer(ctx, request.Live, request.CLOBBaseURL, request.RelayerAPIKey, request.RelayerAddr)
	if err != nil {
		status := http.StatusOK
		if errors.Is(err, engine.ErrInvalidSettings) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"signer":     probe.Signer,
		"address":    probe.Address,
		"api_key":    probe.APIKey,
		"key_count":  probe.KeyCount,
		"latency_ms": probe.LatencyMS,
	})
}

// discoverFunder asks the CLOB which proxy / deposit wallets belong to the
// signer in the request body. It saves nothing and needs no funder, so the
// settings page can fill the field for a wallet that was just connected to
// polymarket.com.
func (h *Handler) discoverFunder(c *gin.Context) {
	var request struct {
		Live        engine.LiveSettings `json:"live"`
		CLOBBaseURL string              `json:"clob_base_url"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Funder 查询请求无效: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	wallets, err := h.engine.DiscoverFunder(ctx, request.Live, request.CLOBBaseURL)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "wallets": wallets})
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
