package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// SIWC 资格闸门管理接口：查看 / 手动复检 / 恢复调度 / 指纹验证 / 汇总。

func (h *OpenAIOAuthHandler) loadSIWCAccount(c *gin.Context) (*service.Account, bool) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return nil, false
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return nil, false
	}
	if account == nil || !account.IsOpenAISIWC() {
		response.BadRequest(c, "account is not a Sign in with ChatGPT account")
		return nil, false
	}
	if h.siwcGate == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "SIWC_GATE_NOT_CONFIGURED", "message": "siwc gate service is not configured"})
		return nil, false
	}
	return account, true
}

func siwcGateView(account *service.Account) *service.SIWCGateState {
	state := service.ReadSIWCGateState(account)
	if state == nil {
		state = &service.SIWCGateState{State: service.SIWCGateStateClear}
	}
	if state.Fingerprint == nil {
		state.Fingerprint = service.ReadSIWCFingerprint(account)
	}
	return state
}

// SIWCGateGet 查看闸门状态
// GET /api/v1/admin/openai/siwc/accounts/:id/gate
func (h *OpenAIOAuthHandler) SIWCGateGet(c *gin.Context) {
	account, ok := h.loadSIWCAccount(c)
	if !ok {
		return
	}
	response.Success(c, siwcGateView(account))
}

// SIWCGateRecheck 手动复检（按邮箱，60 秒冷却）
// POST /api/v1/admin/openai/siwc/accounts/:id/gate/recheck
func (h *OpenAIOAuthHandler) SIWCGateRecheck(c *gin.Context) {
	account, ok := h.loadSIWCAccount(c)
	if !ok {
		return
	}
	state, err := h.siwcGate.Recheck(c.Request.Context(), strings.ToLower(strings.TrimSpace(account.GetCredential("email"))), true)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

// SIWCGateRestore 恢复调度（仅 restorable；先跑指纹）
// POST /api/v1/admin/openai/siwc/accounts/:id/gate/restore
func (h *OpenAIOAuthHandler) SIWCGateRestore(c *gin.Context) {
	account, ok := h.loadSIWCAccount(c)
	if !ok {
		return
	}
	state, err := h.siwcGate.Restore(c.Request.Context(), strings.ToLower(strings.TrimSpace(account.GetCredential("email"))))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

// SIWCFingerprint 手动跑 astra/luna 指纹
// POST /api/v1/admin/openai/siwc/accounts/:id/fingerprint
func (h *OpenAIOAuthHandler) SIWCFingerprint(c *gin.Context) {
	account, ok := h.loadSIWCAccount(c)
	if !ok {
		return
	}
	fp, err := h.siwcGate.Fingerprint(c.Request.Context(), account)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, fp)
}

// SIWCGateSummary 闸门汇总
// GET /api/v1/admin/openai/siwc/gate/summary
func (h *OpenAIOAuthHandler) SIWCGateSummary(c *gin.Context) {
	if h.siwcGate == nil {
		response.Success(c, gin.H{"blocked": 0, "restorable": 0})
		return
	}
	blocked, restorable, err := h.siwcGate.Summary(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"blocked": blocked, "restorable": restorable})
}
