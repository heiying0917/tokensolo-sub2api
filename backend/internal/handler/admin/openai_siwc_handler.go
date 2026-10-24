package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// OpenAISIWCGenerateAuthURLRequest 生成 SIWC 授权链接的请求体。
type OpenAISIWCGenerateAuthURLRequest struct {
	ProxyID     *int64 `json:"proxy_id"`
	RedirectURI string `json:"redirect_uri"`
	AgentName   string `json:"agent_name"`
}

// SIWCGenerateAuthURL 生成 Sign in with ChatGPT 授权链接
// POST /api/v1/admin/openai/siwc/generate-auth-url
func (h *OpenAIOAuthHandler) SIWCGenerateAuthURL(c *gin.Context) {
	if h.siwcOAuthService == nil {
		response.BadRequest(c, "siwc oauth service is not configured")
		return
	}
	var req OpenAISIWCGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// 允许空 body
		req = OpenAISIWCGenerateAuthURLRequest{}
	}
	result, err := h.siwcOAuthService.GenerateAuthURL(c.Request.Context(), &service.OpenAISIWCGenerateAuthURLInput{
		ProxyID:     req.ProxyID,
		RedirectURI: req.RedirectURI,
		AgentName:   req.AgentName,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// OpenAISIWCCallbackFields 是回调的两种提交形态：整条 callback_url，或拆开的 code/state/client_id。
type OpenAISIWCCallbackFields struct {
	SessionID   string `json:"session_id" binding:"required"`
	CallbackURL string `json:"callback_url"`
	Code        string `json:"code"`
	State       string `json:"state"`
	ClientID    string `json:"client_id"`
	ProxyID     *int64 `json:"proxy_id"`
}

// resolveCallbackURL 把拆开的字段拼回 query 串，统一交给 service 解析。
func (f *OpenAISIWCCallbackFields) resolveCallbackURL() string {
	if raw := strings.TrimSpace(f.CallbackURL); raw != "" {
		return raw
	}
	values := url.Values{}
	if v := strings.TrimSpace(f.Code); v != "" {
		values.Set("code", v)
	}
	if v := strings.TrimSpace(f.State); v != "" {
		values.Set("state", v)
	}
	if v := strings.TrimSpace(f.ClientID); v != "" {
		values.Set("client_id", v)
	}
	return values.Encode()
}

// SIWCExchangeCode 两步式：用回调换取令牌信息，不建号
// POST /api/v1/admin/openai/siwc/exchange-code
func (h *OpenAIOAuthHandler) SIWCExchangeCode(c *gin.Context) {
	if h.siwcOAuthService == nil {
		response.BadRequest(c, "siwc oauth service is not configured")
		return
	}
	var req OpenAISIWCCallbackFields
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	tokenInfo, err := h.siwcOAuthService.ExchangeCallback(c.Request.Context(), &service.OpenAISIWCExchangeInput{
		SessionID:   req.SessionID,
		CallbackURL: req.resolveCallbackURL(),
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, tokenInfo)
}

// OpenAISIWCCreateRequest 一步式建号请求：回调字段 + 账号公共字段。
type OpenAISIWCCreateRequest struct {
	OpenAISIWCCallbackFields
	Name                    string         `json:"name"`
	Notes                   *string        `json:"notes"`
	GroupIDs                []int64        `json:"group_ids"`
	Concurrency             *int           `json:"concurrency"`
	Priority                *int           `json:"priority"`
	RateMultiplier          *float64       `json:"rate_multiplier"`
	LoadFactor              *int           `json:"load_factor"`
	ExpiresAt               *int64         `json:"expires_at"`
	AutoPauseOnExpired      *bool          `json:"auto_pause_on_expired"`
	CredentialExtras        map[string]any `json:"credential_extras"`
	Extra                   map[string]any `json:"extra"`
	SkipDefaultGroupBind    *bool          `json:"skip_default_group_bind"`
	ConfirmMixedChannelRisk *bool          `json:"confirm_mixed_channel_risk"`
	// ManagementAccountID 显式指定同邮箱的 Codex OAuth 账号作管理凭据；为空则按邮箱自动匹配。
	ManagementAccountID *int64 `json:"management_account_id"`
	// InheritFromAccountID 从同邮箱已有 SIWC Agent 继承分组、代理、优先级、模型白名单等设置（添加 Agent 用）。
	InheritFromAccountID *int64 `json:"inherit_from_account_id"`
}

// SIWCCreateAccountFromOAuth 一步式：换码并直接创建 SIWC 账号
// POST /api/v1/admin/openai/siwc/create-from-oauth
func (h *OpenAIOAuthHandler) SIWCCreateAccountFromOAuth(c *gin.Context) {
	if h.siwcOAuthService == nil {
		response.BadRequest(c, "siwc oauth service is not configured")
		return
	}
	var req OpenAISIWCCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := service.ValidateOpenAILongContextBillingExtra(service.PlatformOpenAI, req.Extra); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if req.Concurrency != nil && *req.Concurrency < 0 {
		response.BadRequest(c, "concurrency must be >= 0")
		return
	}
	if req.Priority != nil && *req.Priority < 0 {
		response.BadRequest(c, "priority must be >= 0")
		return
	}
	if req.RateMultiplier != nil && *req.RateMultiplier < 0 {
		response.BadRequest(c, "rate_multiplier must be >= 0")
		return
	}
	if req.LoadFactor != nil && *req.LoadFactor > 10000 {
		response.BadRequest(c, "load_factor must be <= 10000")
		return
	}

	var inheritFrom *service.Account
	if req.InheritFromAccountID != nil && *req.InheritFromAccountID > 0 {
		source, srcErr := h.adminService.GetAccount(c.Request.Context(), *req.InheritFromAccountID)
		if srcErr != nil || source == nil || !source.IsOpenAISIWC() {
			response.BadRequest(c, "inherit_from_account_id must be an existing siwc account")
			return
		}
		inheritFrom = source
		applySIWCAgentInheritance(&req, source)
	}

	tokenInfo, err := h.siwcOAuthService.ExchangeCallback(c.Request.Context(), &service.OpenAISIWCExchangeInput{
		SessionID:   req.SessionID,
		CallbackURL: req.resolveCallbackURL(),
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if inheritFrom != nil && !strings.EqualFold(strings.TrimSpace(tokenInfo.Email), strings.TrimSpace(inheritFrom.GetCredential("email"))) {
		response.ErrorFrom(c, infraerrors.New(http.StatusBadRequest, "SIWC_AGENT_EMAIL_MISMATCH",
			"authorized ChatGPT account email does not match this account; sign in with "+inheritFrom.GetCredential("email")))
		return
	}

	credentials := mergeCodexImportMap(
		h.siwcOAuthService.BuildAccountCredentials(tokenInfo),
		sanitizeCodexImportCredentialExtras(req.CredentialExtras),
	)
	var managementAccountID int64
	if req.ManagementAccountID != nil && *req.ManagementAccountID > 0 {
		managementAccountID = *req.ManagementAccountID
	}
	if inheritFrom != nil {
		// 模型白名单 / compact 映射 / UA 属于账号配置，随 Agent 继承
		for _, key := range []string{"model_mapping", "compact_model_mapping", "user_agent"} {
			if _, exists := credentials[key]; !exists {
				if value, ok := inheritFrom.Credentials[key]; ok && value != nil {
					credentials[key] = value
				}
			}
		}
		if managementAccountID == 0 {
			managementAccountID = inheritFrom.GetOpenAISIWCManagementAccountID()
		}
	}
	extra := mergeCodexImportMap(req.Extra, service.BuildOpenAISIWCAccountExtra(managementAccountID))
	extra["imported_at"] = time.Now().UTC().Format(time.RFC3339)
	accountName := buildOpenAISIWCAccountName(req.Name, tokenInfo)
	if inheritFrom != nil && strings.TrimSpace(req.Name) == "" {
		accountName = h.nextSIWCAgentName(c.Request.Context(), inheritFrom)
	}

	concurrency := 3
	if req.Concurrency != nil {
		concurrency = *req.Concurrency
	}
	priority := 50
	if req.Priority != nil {
		priority = *req.Priority
	}
	skipDefaultGroupBind := false
	if req.SkipDefaultGroupBind != nil {
		skipDefaultGroupBind = *req.SkipDefaultGroupBind
	}

	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:                  accountName,
		Notes:                 req.Notes,
		Platform:              service.PlatformOpenAI,
		Type:                  service.AccountTypeOAuth,
		Credentials:           credentials,
		Extra:                 extra,
		ProxyID:               req.ProxyID,
		Concurrency:           concurrency,
		Priority:              priority,
		RateMultiplier:        req.RateMultiplier,
		LoadFactor:            req.LoadFactor,
		GroupIDs:              req.GroupIDs,
		ExpiresAt:             req.ExpiresAt,
		AutoPauseOnExpired:    req.AutoPauseOnExpired,
		SkipDefaultGroupBind:  skipDefaultGroupBind,
		SkipMixedChannelCheck: req.ConfirmMixedChannelRisk != nil && *req.ConfirmMixedChannelRisk,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 同邮箱已有活跃 Agent 时新 Agent 作待命
	h.siwcAgentPool.AssignNewAgentRole(c.Request.Context(), account)
	if refreshed, getErr := h.adminService.GetAccount(c.Request.Context(), account.ID); getErr == nil && refreshed != nil {
		account = refreshed
	}
	response.Success(c, dto.AccountFromService(account))
}

// applySIWCAgentInheritance 用来源 Agent 的设置补齐请求中未显式给出的字段。
func applySIWCAgentInheritance(req *OpenAISIWCCreateRequest, source *service.Account) {
	if req.ProxyID == nil && source.ProxyID != nil {
		proxyID := *source.ProxyID
		req.ProxyID = &proxyID
	}
	if req.GroupIDs == nil {
		req.GroupIDs = append([]int64{}, source.GroupIDs...)
		skip := true
		req.SkipDefaultGroupBind = &skip
	}
	if req.Concurrency == nil {
		concurrency := source.Concurrency
		req.Concurrency = &concurrency
	}
	if req.Priority == nil {
		priority := source.Priority
		req.Priority = &priority
	}
	if req.RateMultiplier == nil && source.RateMultiplier != nil {
		rate := *source.RateMultiplier
		req.RateMultiplier = &rate
	}
	if req.LoadFactor == nil && source.LoadFactor != nil {
		load := *source.LoadFactor
		req.LoadFactor = &load
	}
	if req.ExpiresAt == nil && source.ExpiresAt != nil {
		expires := source.ExpiresAt.Unix()
		req.ExpiresAt = &expires
	}
	if req.AutoPauseOnExpired == nil {
		autoPause := source.AutoPauseOnExpired
		req.AutoPauseOnExpired = &autoPause
	}
}

// nextSIWCAgentName 生成「<邮箱> Agent #n」，n 为组内 Agent 数 + 1。
func (h *OpenAIOAuthHandler) nextSIWCAgentName(ctx context.Context, source *service.Account) string {
	email := strings.TrimSpace(source.GetCredential("email"))
	count := 1
	if h.siwcAgentPool != nil {
		if group, err := h.siwcAgentPool.List(ctx, source.ID); err == nil && group != nil {
			count = len(group.Agents)
		}
	}
	return fmt.Sprintf("%s Agent #%d", email, count+1)
}

// buildOpenAISIWCAccountName 账号名缺省取邮箱，并带上 agent 名以区分同邮箱多份凭据。
func buildOpenAISIWCAccountName(name string, info *service.OpenAISIWCTokenInfo) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	if info != nil {
		if email := strings.TrimSpace(info.Email); email != "" {
			if agent := strings.TrimSpace(info.AgentName); agent != "" {
				return email + " (siwc:" + agent + ")"
			}
			return email + " (siwc)"
		}
		if clientID := strings.TrimSpace(info.ClientID); clientID != "" {
			return clientID
		}
	}
	return "OpenAI SIWC Account"
}
