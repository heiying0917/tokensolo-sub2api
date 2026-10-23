package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
)

// openAISIWCRefreshTimeout 是 SIWC 刷新 / 换码的 HTTP 超时。
// 必须小于统一刷新锁 TTL（60s），否则锁会在上游请求中途过期。
const openAISIWCRefreshTimeout = 30 * time.Second

// OpenAISIWCOAuthClient 封装 SIWC 的 token 端点调用（repository 层实现）。
type OpenAISIWCOAuthClient interface {
	// ExchangeCode 用授权码换 token；clientID 是回调签发的 oaiapp_* 值。
	ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, clientID, proxyURL string) (*openai.TokenResponse, error)
	// Refresh 用 refresh_token 换新 token；上游一次性轮转 RT，调用方必须串行化。
	Refresh(ctx context.Context, clientID, refreshToken, proxyURL string) (*openai.TokenResponse, error)
}

// OpenAISIWCOAuthService 处理 SIWC 授权与刷新。
//
// 与 OpenAIOAuthService 分开实现：后者的 enrichTokenInfo 会拿 access_token
// 打 chatgpt.com 补 plan_type / 关训练，SIWC 令牌打 chatgpt.com 会 401，必须绕开。
type OpenAISIWCOAuthService struct {
	proxyRepo    ProxyRepository
	sessionStore OpenAIOAuthSessionStore
	client       OpenAISIWCOAuthClient
	accountRepo  AccountRepository
}

// NewOpenAISIWCOAuthService 构造 SIWC 授权 service。sessionStore 为 nil 时回退进程内实现。
func NewOpenAISIWCOAuthService(
	proxyRepo ProxyRepository,
	client OpenAISIWCOAuthClient,
	sessionStore OpenAIOAuthSessionStore,
	accountRepo AccountRepository,
) *OpenAISIWCOAuthService {
	if sessionStore == nil {
		sessionStore = newMemoryOpenAIOAuthSessionStore()
	}
	return &OpenAISIWCOAuthService{
		proxyRepo:    proxyRepo,
		sessionStore: sessionStore,
		client:       client,
		accountRepo:  accountRepo,
	}
}

// OpenAISIWCGenerateAuthURLInput 生成授权链接的入参。
type OpenAISIWCGenerateAuthURLInput struct {
	ProxyID     *int64
	RedirectURI string
	AgentName   string
}

// OpenAISIWCAuthURLResult 授权链接结果；State 回给前端用于本地校验回调。
type OpenAISIWCAuthURLResult struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	ExpiresIn int64  `json:"expires_in"`
}

func (s *OpenAISIWCOAuthService) resolveProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil || s.proxyRepo == nil {
		return "", nil
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		return "", infraerrors.Newf(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_NOT_FOUND", "proxy not found: %v", err)
	}
	if proxy == nil {
		return "", nil
	}
	return proxy.URL(), nil
}

// GenerateAuthURL 生成 SIWC 授权链接并把会话写入（Redis）存储。
func (s *OpenAISIWCOAuthService) GenerateAuthURL(ctx context.Context, in *OpenAISIWCGenerateAuthURLInput) (*OpenAISIWCAuthURLResult, error) {
	if in == nil {
		in = &OpenAISIWCGenerateAuthURLInput{}
	}
	state, err := openai.GenerateRandomBase64URL()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_STATE_FAILED", "failed to generate state: %v", err)
	}
	nonce, err := openai.GenerateRandomBase64URL()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_NONCE_FAILED", "failed to generate nonce: %v", err)
	}
	codeVerifier, err := openai.GenerateRandomBase64URL()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_VERIFIER_FAILED", "failed to generate code verifier: %v", err)
	}
	sessionID, err := openai.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_FAILED", "failed to generate session ID: %v", err)
	}
	proxyURL, err := s.resolveProxyURL(ctx, in.ProxyID)
	if err != nil {
		return nil, err
	}

	redirectURI := strings.TrimSpace(in.RedirectURI)
	if redirectURI == "" {
		redirectURI = openai.SIWCDefaultRedirectURI
	}
	agentName := strings.TrimSpace(in.AgentName)
	if agentName == "" {
		agentName = openai.SIWCDefaultAgentName
	}
	deviceID := uuid.NewString()

	authURL, err := openai.BuildSIWCAuthorizationURL(openai.SIWCAuthorizationParams{
		State:         state,
		Nonce:         nonce,
		CodeChallenge: openai.GenerateCodeChallenge(codeVerifier),
		DeviceID:      deviceID,
		AgentName:     agentName,
		RedirectURI:   redirectURI,
	})
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_SIWC_AUTH_URL_FAILED", "failed to build siwc auth url: %v", err)
	}

	session := &openai.OAuthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		ClientID:     openai.SIWCDynamicClientID,
		ProxyURL:     proxyURL,
		RedirectURI:  redirectURI,
		CreatedAt:    time.Now(),
		AuthMode:     openai.SIWCAuthModeSession,
		Nonce:        nonce,
		DeviceID:     deviceID,
		AgentName:    agentName,
	}
	if err := s.sessionStore.Save(ctx, sessionID, session, openai.SessionTTL); err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_FAILED", "failed to store oauth session: %v", err)
	}

	return &OpenAISIWCAuthURLResult{
		AuthURL:   authURL,
		SessionID: sessionID,
		State:     state,
		ExpiresIn: int64(openai.SessionTTL / time.Second),
	}, nil
}

// OpenAISIWCExchangeInput 换码入参：CallbackURL 是用户粘贴的整条回调 URL。
type OpenAISIWCExchangeInput struct {
	SessionID   string
	CallbackURL string
	ProxyID     *int64
}

// OpenAISIWCTokenInfo 换码 / 刷新得到的令牌信息。
type OpenAISIWCTokenInfo struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	IDToken      string   `json:"id_token,omitempty"`
	ClientID     string   `json:"client_id"`
	ExpiresIn    int64    `json:"expires_in"`
	ExpiresAt    int64    `json:"expires_at"`
	Scopes       []string `json:"scopes"`
	Email        string   `json:"email,omitempty"`
	Subject      string   `json:"subject,omitempty"`
	Issuer       string   `json:"issuer,omitempty"`
	PlanType     string   `json:"plan_type,omitempty"`
	DeviceID     string   `json:"device_id,omitempty"`
	AgentName    string   `json:"agent_name,omitempty"`
}

// ExchangeCallback 校验回调并换取令牌。
func (s *OpenAISIWCOAuthService) ExchangeCallback(ctx context.Context, in *OpenAISIWCExchangeInput) (*OpenAISIWCTokenInfo, error) {
	if in == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_SIWC_INPUT_REQUIRED", "exchange input is required")
	}
	if s.client == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_SIWC_CLIENT_MISSING", "siwc oauth client is not configured")
	}
	session, err := s.sessionStore.Get(ctx, in.SessionID)
	if err != nil || session == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_NOT_FOUND", "session not found or expired")
	}
	if session.AuthMode != openai.SIWCAuthModeSession {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_MODE_MISMATCH", "session belongs to a different oauth flow")
	}

	callback, err := openai.ParseSIWCCallbackURL(in.CallbackURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_SIWC_INVALID_CALLBACK", "invalid siwc callback: %v", err)
	}
	if callback.Error != "" {
		msg := callback.Error
		if callback.ErrorDescription != "" {
			msg += ": " + callback.ErrorDescription
		}
		return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_SIWC_AUTHORIZATION_DENIED", "siwc authorization failed: %s", msg)
	}
	if subtle.ConstantTimeCompare([]byte(callback.State), []byte(session.State)) != 1 {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_STATE", "invalid oauth state")
	}

	proxyURL := session.ProxyURL
	if in.ProxyID != nil {
		resolved, err := s.resolveProxyURL(ctx, in.ProxyID)
		if err != nil {
			return nil, err
		}
		proxyURL = resolved
	}

	tokenResp, err := s.client.ExchangeCode(ctx, callback.Code, session.CodeVerifier, session.RedirectURI, callback.ClientID, proxyURL)
	if err != nil {
		return nil, err
	}
	if err := openai.ValidateSIWCTokenResponse(tokenResp, true); err != nil {
		if errors.Is(err, openai.ErrSIWCScopeNotGranted) {
			return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_SIWC_SCOPE_NOT_GRANTED", "siwc grant did not include "+openai.SIWCRequiredScope+"; the ChatGPT account or workspace must allow token sharing")
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_SIWC_TOKEN_INVALID", "invalid siwc token response: %v", err)
	}

	claims, err := openai.ParseIDToken(tokenResp.IDToken)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_SIWC_ID_TOKEN_INVALID", "invalid siwc id_token: %v", err)
	}
	if session.Nonce != "" && subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(session.Nonce)) != 1 {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_SIWC_NONCE_MISMATCH", "siwc id_token nonce does not match session")
	}

	// 换码成功后删除会话；删除失败只影响重放窗口，不阻断建号。
	if err := s.sessionStore.Delete(ctx, in.SessionID); err != nil {
		slog.Warn("openai_siwc_session_delete_failed", "session_id", in.SessionID, "error", err)
	}

	now := time.Now()
	info := &OpenAISIWCTokenInfo{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ClientID:     callback.ClientID,
		ExpiresIn:    tokenResp.ExpiresIn,
		ExpiresAt:    now.Unix() + tokenResp.ExpiresIn,
		Scopes:       openai.SplitScopes(tokenResp.Scope),
		Email:        strings.TrimSpace(claims.Email),
		Subject:      strings.TrimSpace(claims.Sub),
		Issuer:       strings.TrimSpace(claims.Iss),
		DeviceID:     session.DeviceID,
		AgentName:    session.AgentName,
	}
	if claims.OpenAIAuth != nil {
		info.PlanType = strings.TrimSpace(claims.OpenAIAuth.ChatGPTPlanType)
	}
	// SIWC 的 id_token 通常不带套餐；借同邮箱 Codex 管理凭据回填 plan_type。
	if info.PlanType == "" && s.accountRepo != nil && info.Email != "" {
		probe := &Account{
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{openAIAuthModeCredentialKey: OpenAIAuthModeSIWC, "email": info.Email},
		}
		if mgmt, mgmtErr := resolveOpenAISIWCManagementAccount(ctx, s.accountRepo, probe); mgmtErr == nil && mgmt != nil {
			info.PlanType = strings.TrimSpace(mgmt.GetCredential("plan_type"))
		}
	}
	return info, nil
}

// BuildAccountCredentials 把令牌信息组装成账号 credentials。
func (s *OpenAISIWCOAuthService) BuildAccountCredentials(info *OpenAISIWCTokenInfo) map[string]any {
	if info == nil {
		return nil
	}
	creds := map[string]any{
		openAIAuthModeCredentialKey: OpenAIAuthModeSIWC,
		"client_id":                 strings.TrimSpace(info.ClientID),
		"access_token":              info.AccessToken,
	}
	if strings.TrimSpace(info.RefreshToken) != "" {
		creds["refresh_token"] = info.RefreshToken
	}
	if strings.TrimSpace(info.IDToken) != "" {
		creds["id_token"] = info.IDToken
	}
	if info.ExpiresAt > 0 {
		creds["expires_at"] = time.Unix(info.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	if info.Email != "" {
		creds["email"] = info.Email
	}
	if info.PlanType != "" {
		creds["plan_type"] = info.PlanType
	}
	if len(info.Scopes) > 0 {
		creds[openAISIWCScopesCredentialKey] = append([]string(nil), info.Scopes...)
	}
	if info.DeviceID != "" {
		creds[openAISIWCDeviceIDCredentialKey] = info.DeviceID
	}
	if info.AgentName != "" {
		creds[openAISIWCAgentNameCredentialKey] = info.AgentName
	}
	if info.Issuer != "" {
		creds[openAISIWCIssuerCredentialKey] = info.Issuer
	}
	if info.Subject != "" {
		creds[openAISIWCSubjectCredentialKey] = info.Subject
	}
	return creds
}

// BuildAccountExtra 返回 SIWC 账号创建时预置的 extra 键。
func BuildOpenAISIWCAccountExtra(managementAccountID int64) map[string]any {
	extra := map[string]any{
		"import_source":        OpenAISIWCImportSource,
		"auth_provider":        OpenAISIWCImportSource,
		"privacy_mode":         OpenAIPrivacyModeSIWCNotApplicable,
		"openai_ws_force_http": true,
	}
	if managementAccountID > 0 {
		extra[openAISIWCManagementAccountExtraKey] = managementAccountID
	}
	return extra
}

// RefreshCredential 刷新 SIWC 账号令牌，返回需要合并进 credentials 的增量键。
//
// 上游一次性轮转 refresh_token：新 RT 一旦签发旧 RT 立即失效，所以
// HTTP 调用脱离请求 ctx（WithoutCancel），避免客户端断连把已轮转的凭据丢掉。
func (s *OpenAISIWCOAuthService) RefreshCredential(ctx context.Context, account *Account) (map[string]any, error) {
	if account == nil || !account.IsOpenAISIWC() {
		return nil, errors.New("not an openai siwc account")
	}
	if s.client == nil {
		return nil, errors.New("siwc oauth client is not configured")
	}
	clientID := account.GetOpenAISIWCClientID()
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	if clientID == "" {
		return nil, errors.New("siwc credential is missing client_id; re-authorize")
	}
	if refreshToken == "" {
		return nil, errors.New("siwc credential is missing refresh_token; re-authorize")
	}

	proxyURL := ""
	if account.ProxyID != nil && s.proxyRepo != nil {
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAISIWCRefreshTimeout)
	defer cancel()
	tokenResp, err := s.client.Refresh(refreshCtx, clientID, refreshToken, proxyURL)
	if err != nil {
		return nil, err
	}
	if err := openai.ValidateSIWCTokenResponse(tokenResp, false); err != nil {
		return nil, err
	}

	now := time.Now()
	creds := map[string]any{
		"access_token": tokenResp.AccessToken,
		"expires_at":   now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
	}
	if strings.TrimSpace(tokenResp.RefreshToken) != "" {
		creds["refresh_token"] = tokenResp.RefreshToken
	} else {
		slog.Warn("openai_siwc_refresh_without_new_refresh_token", "account_id", account.ID)
	}
	if strings.TrimSpace(tokenResp.IDToken) != "" {
		creds["id_token"] = tokenResp.IDToken
	}
	if scopes := openai.SplitScopes(tokenResp.Scope); len(scopes) > 0 {
		creds[openAISIWCScopesCredentialKey] = scopes
	}
	return creds, nil
}

// ResolveManagementAccount 解析 SIWC 账号的管理凭据（额度 / 模型清单用）。
func (s *OpenAISIWCOAuthService) ResolveManagementAccount(ctx context.Context, account *Account) (*Account, error) {
	return resolveOpenAISIWCManagementAccount(ctx, s.accountRepo, account)
}
