package admin

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

func (h *OpenAIOAuthHandler) requireSIWCAgentPool(c *gin.Context) (*service.SIWCAgentPoolService, int64, bool) {
	if h.siwcAgentPool == nil {
		response.ErrorFrom(c, infraerrors.New(http.StatusInternalServerError, "SIWC_AGENT_NOT_CONFIGURED", "siwc agent pool is not configured"))
		return nil, 0, false
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return nil, 0, false
	}
	return h.siwcAgentPool, accountID, true
}

// SIWCAgentsList 返回 SIWC 账号所在 ChatGPT 账号下的全部 Agent。
// GET /api/v1/admin/openai/siwc/accounts/:id/agents
func (h *OpenAIOAuthHandler) SIWCAgentsList(c *gin.Context) {
	pool, accountID, ok := h.requireSIWCAgentPool(c)
	if !ok {
		return
	}
	group, err := pool.List(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, group)
}

// SIWCAgentActivate 把指定 Agent 设为活跃，其余同邮箱 Agent 转为待命。
// POST /api/v1/admin/openai/siwc/accounts/:id/agents/activate
func (h *OpenAIOAuthHandler) SIWCAgentActivate(c *gin.Context) {
	pool, accountID, ok := h.requireSIWCAgentPool(c)
	if !ok {
		return
	}
	// 管理端请求本身已由审计中间件记录操作人，这里只标来源
	group, err := pool.Activate(c.Request.Context(), accountID, "admin")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, group)
}

// SIWCReauthFromOAuth 用新的 SIWC 授权替换单个 Agent 的凭据，保留角色与账号设置。
// POST /api/v1/admin/openai/siwc/accounts/:id/reauth-from-oauth
func (h *OpenAIOAuthHandler) SIWCReauthFromOAuth(c *gin.Context) {
	if h.siwcOAuthService == nil {
		response.BadRequest(c, "siwc oauth service is not configured")
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req OpenAISIWCCallbackFields
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	ctx := c.Request.Context()
	existing, err := h.adminService.GetAccount(ctx, accountID)
	if err != nil || existing == nil {
		response.NotFound(c, "Account not found")
		return
	}
	if !existing.IsOpenAISIWC() {
		response.BadRequest(c, "account is not a siwc agent")
		return
	}
	proxyID := req.ProxyID
	if proxyID == nil {
		proxyID = existing.ProxyID
	}
	tokenInfo, err := h.siwcOAuthService.ExchangeCallback(ctx, &service.OpenAISIWCExchangeInput{
		SessionID:   req.SessionID,
		CallbackURL: req.resolveCallbackURL(),
		ProxyID:     proxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 防串号：重新授权必须登录同一个 ChatGPT 账号
	if !strings.EqualFold(strings.TrimSpace(tokenInfo.Email), strings.TrimSpace(existing.GetCredential("email"))) {
		response.ErrorFrom(c, infraerrors.New(http.StatusBadRequest, "SIWC_AGENT_EMAIL_MISMATCH",
			"authorized ChatGPT account email does not match this agent; sign in with "+existing.GetCredential("email")))
		return
	}

	// 新令牌覆盖认证字段，模型白名单等账号配置随旧凭据保留
	credentials := service.MergeCredentials(existing.Credentials, h.siwcOAuthService.BuildAccountCredentials(tokenInfo))
	updated, err := h.adminService.UpdateAccount(ctx, accountID, &service.UpdateAccountInput{
		Type:        service.AccountTypeOAuth,
		Credentials: credentials,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 资格闸门扣住时只换凭据，不清错误（闸门语义优先，等复检 + 指纹恢复）
	if gate := service.ReadSIWCGateState(existing); gate == nil || !gate.IsGuarded() {
		if cleared, clearErr := h.adminService.ClearAccountError(ctx, accountID); clearErr != nil {
			slog.Warn("siwc_reauth.clear_error_failed", "account_id", accountID, "error", clearErr)
		} else if cleared != nil {
			updated = cleared
		}
		// 活跃 Agent 重新授权后恢复调度（SetError 时 schedulable 已被置 false）；待命保持待命
		if h.siwcAgentPool != nil && !updated.IsOpenAISIWCStandby() {
			if _, actErr := h.siwcAgentPool.Activate(ctx, accountID, "admin"); actErr != nil {
				slog.Warn("siwc_reauth.activate_failed", "account_id", accountID, "error", actErr)
			}
		}
	}
	if refreshed, getErr := h.adminService.GetAccount(ctx, accountID); getErr == nil && refreshed != nil {
		updated = refreshed
	}
	response.Success(c, dto.AccountFromService(updated))
}

// SIWCAgentRemove 在 Agent 管理里删除一个 Agent；删主行时下一个 Agent 自动升为主行。
// POST /api/v1/admin/openai/siwc/accounts/:id/agents/remove
func (h *OpenAIOAuthHandler) SIWCAgentRemove(c *gin.Context) {
	pool, accountID, ok := h.requireSIWCAgentPool(c)
	if !ok {
		return
	}
	group, err := pool.RemoveAgent(c.Request.Context(), accountID, h.adminService.DeleteAccount)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, group)
}
