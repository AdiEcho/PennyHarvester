package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"pennyharvester/backend/internal/engine"

	"github.com/gin-gonic/gin"
)

const (
	chainOperationDefaultLimit = 50
	chainOperationMaxLimit     = 200
)

// chainOperations lists the durable transaction journal. The UI uses
// status=unknown to show the backlog an operator has to check on-chain; other
// states stay visible for auditing. Nothing here talks to a venue.
func (h *Handler) chainOperations(c *gin.Context) {
	v, ok := venueFromQuery(c)
	if !ok {
		return
	}
	accountID, ok := chainOperationAccountID(c)
	if !ok {
		return
	}
	status := strings.TrimSpace(c.DefaultQuery("status", ""))
	if strings.EqualFold(status, "all") {
		status = ""
	}
	if status != "" && !isChainOperationStatus(status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status 仅支持 intent、signed、broadcast、confirmed、reverted、unknown 或 all"})
		return
	}
	limit := chainOperationDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > chainOperationMaxLimit {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit 须为 1–200"})
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset 须为非负整数"})
			return
		}
		offset = parsed
	}
	page, err := h.engine.ListChainOperations(c.Request.Context(), engine.ChainOperationFilter{
		AccountID: accountID,
		Venue:     v,
		Status:    status,
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// resolveChainOperation records an operator's verified outcome for one unknown
// row. It deliberately accepts no nonce and triggers no broadcast: a human has
// already inspected the chain, and the transaction identity must not change.
func (h *Handler) resolveChainOperation(c *gin.Context) {
	id, ok := parseChainOperationID(c)
	if !ok {
		return
	}
	var request struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
		TxHash string `json:"tx_hash"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "解决请求 JSON 无效: " + err.Error()})
		return
	}
	operation, err := h.engine.ResolveChainOperation(c.Request.Context(), id, request.Status, request.Reason, request.Actor, request.TxHash)
	if err != nil {
		status := chainOperationErrorStatus(err)
		message := err.Error()
		if status == http.StatusInternalServerError {
			// Do not echo wrapped SQL/store details to the browser; the operator
			// only needs to know the request failed and can retry.
			message = "链上操作失败"
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	c.JSON(http.StatusOK, operation)
}

func parseChainOperationID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "链上操作 ID 无效"})
		return 0, false
	}
	return id, true
}

// chainOperationAccountID keeps 0 as the explicit "all accounts" sentinel used
// by ChainOperationFilter. The orders endpoint has no such sentinel, so it uses
// accountIDFromQuery instead.
func chainOperationAccountID(c *gin.Context) (int64, bool) {
	raw := strings.TrimSpace(c.Query("account_id"))
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id 无效"})
		return 0, false
	}
	return id, true
}

func isChainOperationStatus(status string) bool {
	switch status {
	case "intent", "signed", "broadcast", "confirmed", "reverted", "unknown":
		return true
	default:
		return false
	}
}

// chainOperationErrorStatus keeps "not found", "already final" and "bad input"
// distinguishable so the operator knows whether to fix the request, refresh the
// list, or accept that another resolution won.
func chainOperationErrorStatus(err error) int {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, engine.ErrChainOperationConflict), errors.Is(err, engine.ErrChainOperationActive):
		return http.StatusConflict
	case errors.Is(err, engine.ErrChainOperationInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
