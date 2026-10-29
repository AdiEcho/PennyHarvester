package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"pennyharvester/backend/internal/engine"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	engine *engine.Engine
}

func NewRouter(engine *engine.Engine, webDir string) *gin.Engine {
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery(), securityHeaders())
	h := &Handler{engine: engine}
	v1 := router.Group("/api/v1")
	v1.Use(maxBodySize(1 << 20))
	{
		v1.GET("/health", h.health)
		v1.GET("/accounts", h.accounts)
		v1.POST("/accounts", h.createAccount)
		v1.PATCH("/accounts/:id", h.updateAccount)
		v1.DELETE("/accounts/:id", h.deleteAccount)
		v1.GET("/dashboard", h.dashboard)
		v1.GET("/opportunities", h.opportunities)
		v1.POST("/scan", h.scan)
		v1.GET("/positions", h.positions)
		v1.GET("/orders", h.orders)
		v1.GET("/settings", h.settings)
		v1.PUT("/settings", h.updateSettings)
		v1.POST("/engine/toggle", h.toggleEngine)
		v1.POST("/accounts/:id/orders", h.placeOrder)
		v1.POST("/paper/orders", h.paperOrderCompat)
	}
	if webDir != "" {
		serveWeb(router, webDir)
	}
	return router
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		if c.Request.URL.Path == "/api/v1" || len(c.Request.URL.Path) > 8 && c.Request.URL.Path[:8] == "/api/v1/" {
			c.Header("Cache-Control", "no-store")
		}
		c.Next()
	}
}

func maxBodySize(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		c.Next()
	}
}

func (h *Handler) health(c *gin.Context) {
	defaultAccount, err := h.engine.ResolveAccount(c.Request.Context(), 0)
	mode := "live"
	liveConfigured := false
	if err == nil {
		mode = string(defaultAccount.Mode)
		liveConfigured = defaultAccount.LiveConfigured
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "service": "pennyharvester",
		"mode": mode, "live_configured": liveConfigured,
		"default_account_mode": mode, "time": time.Now().UTC(),
	})
}

func (h *Handler) accounts(c *gin.Context) {
	accounts, err := h.engine.Accounts(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, accounts)
}

func (h *Handler) createAccount(c *gin.Context) {
	var request struct {
		Name string             `json:"name" binding:"required"`
		Mode engine.AccountMode `json:"mode" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户字段无效: " + err.Error()})
		return
	}
	account, err := h.engine.CreateAccount(c.Request.Context(), request.Name, request.Mode)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, account)
}

func (h *Handler) updateAccount(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var request struct {
		Name      *string `json:"name"`
		AutoTrade *bool   `json:"auto_trade"`
		IsDefault *bool   `json:"is_default"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户更新字段无效: " + err.Error()})
		return
	}
	account, err := h.engine.UpdateAccount(c.Request.Context(), id, request.Name, request.AutoTrade, request.IsDefault)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, account)
}

func (h *Handler) deleteAccount(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if err := h.engine.DeleteAccount(c.Request.Context(), id); err != nil {
		respondError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) dashboard(c *gin.Context) {
	accountID, ok := accountIDFromQuery(c)
	if !ok {
		return
	}
	dashboard, err := h.engine.Dashboard(c.Request.Context(), accountID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, dashboard)
}

func (h *Handler) opportunities(c *gin.Context) {
	c.JSON(http.StatusOK, h.engine.Opportunities())
}

func (h *Handler) scan(c *gin.Context) {
	if err := h.engine.Scan(c.Request.Context()); err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.engine.Opportunities())
}

func (h *Handler) positions(c *gin.Context) {
	accountID, ok := accountIDFromQuery(c)
	if !ok {
		return
	}
	status := c.DefaultQuery("status", "open")
	if status != "open" && status != "won" && status != "lost" && status != "all" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status 仅支持 open、won、lost 或 all"})
		return
	}
	positions, err := h.engine.Positions(c.Request.Context(), accountID, status)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, positions)
}

func (h *Handler) orders(c *gin.Context) {
	accountID, ok := accountIDFromQuery(c)
	if !ok {
		return
	}
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit 须为 1–500"})
			return
		}
		limit = parsed
	}
	orders, err := h.engine.Orders(c.Request.Context(), accountID, limit)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, orders)
}

func (h *Handler) settings(c *gin.Context) {
	c.JSON(http.StatusOK, h.engine.Settings())
}

func (h *Handler) updateSettings(c *gin.Context) {
	var settings engine.Settings
	if err := c.ShouldBindJSON(&settings); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "设置 JSON 无效: " + err.Error()})
		return
	}
	if err := settings.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.engine.UpdateSettings(c.Request.Context(), settings); err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.engine.Settings())
}

func (h *Handler) toggleEngine(c *gin.Context) {
	var request struct {
		Running *bool `json:"running" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "running 必须是布尔值"})
		return
	}
	c.JSON(http.StatusOK, h.engine.SetRunning(*request.Running))
}

func (h *Handler) placeOrder(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var request engine.OrderRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单字段无效: " + err.Error()})
		return
	}
	position, err := h.engine.PlaceOrder(c.Request.Context(), id, request)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, position)
}

// paperOrderCompat keeps the pre-account endpoint usable by resolving the
// first paper account. New clients should use /accounts/:id/orders.
func (h *Handler) paperOrderCompat(c *gin.Context) {
	accounts, err := h.engine.Accounts(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	for _, account := range accounts.Items {
		if account.Mode == engine.AccountModePaper {
			var request engine.OrderRequest
			if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "订单字段无效: " + bindErr.Error()})
				return
			}
			position, placeErr := h.engine.PlaceOrder(c.Request.Context(), account.ID, request)
			if placeErr != nil {
				respondError(c, placeErr)
				return
			}
			c.JSON(http.StatusCreated, position)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "没有可用的模拟账户"})
}

func parseID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户 ID 无效"})
		return 0, false
	}
	return id, true
}

func accountIDFromQuery(c *gin.Context) (int64, bool) {
	raw := c.Query("account_id")
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id 无效"})
		return 0, false
	}
	return id, true
}

func respondError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, engine.ErrInvalidOrder), errors.Is(err, engine.ErrNotFound):
		status = http.StatusBadRequest
	case errors.Is(err, engine.ErrAccountNotFound):
		status = http.StatusNotFound
	case errors.Is(err, engine.ErrAccountConflict):
		status = http.StatusConflict
	case errors.Is(err, engine.ErrLiveUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, engine.ErrUpstream):
		status = http.StatusBadGateway
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

func serveWeb(router *gin.Engine, webDir string) {
	index := filepath.Join(webDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		return
	}
	assets := filepath.Join(webDir, "assets")
	if _, err := os.Stat(assets); err == nil {
		router.Static("/assets", assets)
	}
	router.GET("/", func(c *gin.Context) { c.File(index) })
	router.NoRoute(func(c *gin.Context) {
		if len(c.Request.URL.Path) >= 4 && c.Request.URL.Path[:4] == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"error": "API 路径不存在"})
			return
		}
		c.File(index)
	})
}
