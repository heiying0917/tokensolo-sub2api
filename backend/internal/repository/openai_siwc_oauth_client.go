package repository

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/imroc/req/v3"
)

// openAISIWCClientTimeout 必须小于统一刷新锁 TTL（60s）。
const openAISIWCClientTimeout = 30 * time.Second

// NewOpenAISIWCOAuthClient 构造 SIWC token 端点客户端。
func NewOpenAISIWCOAuthClient() service.OpenAISIWCOAuthClient {
	return &openaiSIWCOAuthClient{tokenURL: openai.SIWCTokenURL}
}

type openaiSIWCOAuthClient struct {
	tokenURL string
}

func createOpenAISIWCReqClient(proxyURL string) (*req.Client, error) {
	return getSharedReqClient(reqClientOptions{
		ProxyURL: proxyURL,
		Timeout:  openAISIWCClientTimeout,
	})
}

// postSIWCTokenForm 发送表单并解析 token 响应。
// 与 Codex 流程不同：不设 Codex UA / originator（SIWC 不做客户端伪装），
// 非 2xx 时把状态码与 body 放进错误消息，让上层按 refresh_token_reused /
// invalid_grant 等关键字做不可重试判定。
func (c *openaiSIWCOAuthClient) postSIWCTokenForm(ctx context.Context, form url.Values, proxyURL, failCode, failPrefix string) (*openai.TokenResponse, error) {
	client, err := createOpenAISIWCReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	var tokenResp openai.TokenResponse
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("Accept", "application/json").
		SetFormDataFromValues(form).
		SetSuccessResult(&tokenResp).
		Post(c.tokenURL)
	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}
	if !resp.IsSuccessState() {
		return nil, infraerrors.Newf(http.StatusBadGateway, failCode, "%s: status %d, body: %s", failPrefix, resp.StatusCode, strings.TrimSpace(resp.String()))
	}
	return &tokenResp, nil
}

func (c *openaiSIWCOAuthClient) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, clientID, proxyURL string) (*openai.TokenResponse, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_SIWC_CLIENT_ID_REQUIRED", "siwc issued client_id is required")
	}
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = openai.SIWCDefaultRedirectURI
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", strings.TrimSpace(code))
	form.Set("code_verifier", codeVerifier)
	form.Set("redirect_uri", strings.TrimSpace(redirectURI))
	form.Set("resource", openai.SIWCResource)
	return c.postSIWCTokenForm(ctx, form, proxyURL, "OPENAI_SIWC_TOKEN_EXCHANGE_FAILED", "siwc token exchange failed")
}

func (c *openaiSIWCOAuthClient) Refresh(ctx context.Context, clientID, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	clientID = strings.TrimSpace(clientID)
	refreshToken = strings.TrimSpace(refreshToken)
	if clientID == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_SIWC_CLIENT_ID_REQUIRED", "siwc issued client_id is required")
	}
	if refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_SIWC_REFRESH_TOKEN_REQUIRED", "siwc refresh_token is required")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)
	form.Set("resource", openai.SIWCResource)
	// 刻意不带 scope：SIWC 刷新由签发的 client_id 决定授权范围。
	return c.postSIWCTokenForm(ctx, form, proxyURL, "OPENAI_SIWC_TOKEN_REFRESH_FAILED", "siwc token refresh failed")
}
