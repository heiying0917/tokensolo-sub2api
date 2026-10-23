package openai

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Sign in with ChatGPT（SIWC）协议常量。
// 与 Codex CLI OAuth 共用 auth.openai.com 签发方，但端点、client、scope 均不同：
//   - 授权走动态客户端注册：授权 URL 用 dynamic_agent_client，回调签发 oaiapp_* client_id，
//     之后换码与刷新都必须用签发值；
//   - scope 必须带 chatgpt.tokens.use.direct，否则拿到的令牌不能用订阅额度；
//   - 令牌只能打 api.openai.com/v1，打 chatgpt.com 会 401 rejected_by_access_enforcement。
const (
	SIWCAuthorizeURL         = "https://auth.openai.com/api/accounts/authorize"
	SIWCTokenURL             = "https://auth.openai.com/api/accounts/oauth/token"
	SIWCDynamicClientID      = "dynamic_agent_client"
	SIWCResource             = "https://api.openai.com/v1"
	SIWCScope                = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	SIWCRequiredScope        = "chatgpt.tokens.use.direct"
	SIWCDefaultRedirectURI   = "http://127.0.0.1:1455/auth/callback"
	SIWCIssuedClientIDPrefix = "oaiapp_"
	SIWCDefaultAgentName     = "sub2api"
	// SIWCAuthModeSession 标记授权会话属于 SIWC 流程，与 Codex 会话区分。
	SIWCAuthModeSession = "siwc"
)

var siwcUUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ErrSIWCScopeNotGranted 表示 token 响应缺少 chatgpt.tokens.use.direct。
var ErrSIWCScopeNotGranted = errors.New("siwc grant did not include " + SIWCRequiredScope)

// IsValidUUID 报告 raw 是否为标准 36 位 UUID。
func IsValidUUID(raw string) bool {
	return siwcUUIDPattern.MatchString(strings.TrimSpace(raw))
}

// GenerateRandomBase64URL 生成 32 字节随机数的 base64url（无填充）编码。
// SIWC 的 verifier / state / nonce 都用这种编码（CLIProxyAPI 与 cpa 工具包一致），
// 与 Codex 流程的 hex verifier 不同。
func GenerateRandomBase64URL() (string, error) {
	raw, err := GenerateRandomBytes(32)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// SIWCAuthorizationParams 构造授权 URL 所需参数。
type SIWCAuthorizationParams struct {
	State         string
	Nonce         string
	CodeChallenge string
	DeviceID      string // ext_agent_host_id = urn:uuid:<device_id>，可为空
	AgentName     string // agent_name_hint，空则用 SIWCDefaultAgentName
	RedirectURI   string // 空则用 SIWCDefaultRedirectURI
}

// BuildSIWCAuthorizationURL 构造 SIWC 授权 URL，参数集合与 CLIProxyAPI 对齐。
func BuildSIWCAuthorizationURL(p SIWCAuthorizationParams) (string, error) {
	state := strings.TrimSpace(p.State)
	nonce := strings.TrimSpace(p.Nonce)
	challenge := strings.TrimSpace(p.CodeChallenge)
	if state == "" {
		return "", errors.New("siwc state is required")
	}
	if nonce == "" {
		return "", errors.New("siwc nonce is required")
	}
	if challenge == "" {
		return "", errors.New("siwc code challenge is required")
	}
	redirectURI := strings.TrimSpace(p.RedirectURI)
	if redirectURI == "" {
		redirectURI = SIWCDefaultRedirectURI
	}
	agentName := strings.TrimSpace(p.AgentName)
	if agentName == "" {
		agentName = SIWCDefaultAgentName
	}

	params := url.Values{}
	params.Set("client_id", SIWCDynamicClientID)
	params.Set("agent_name_hint", agentName)
	if deviceID := strings.ToLower(strings.TrimSpace(p.DeviceID)); deviceID != "" {
		if !IsValidUUID(deviceID) {
			return "", fmt.Errorf("siwc device id must be a uuid, got %q", p.DeviceID)
		}
		params.Set("ext_agent_host_id", "urn:uuid:"+deviceID)
	}
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("resource", SIWCResource)
	params.Set("scope", SIWCScope)
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("nonce", nonce)
	return SIWCAuthorizeURL + "?" + params.Encode(), nil
}

// SIWCCallback 是从回调 URL 解析出的字段。
type SIWCCallback struct {
	Code             string
	State            string
	ClientID         string
	Error            string
	ErrorDescription string
}

// ParseSIWCCallbackURL 解析用户粘贴回来的回调 URL（或裸 query 串）。
// 回调带 code / state / client_id 三个必需参数；client_id 必须是签发的 oaiapp_* 值。
// 带 error= 的回调原样返回错误信息，不再校验其它字段。
func ParseSIWCCallbackURL(raw string) (*SIWCCallback, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("siwc callback url is required")
	}
	var query url.Values
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "/") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse siwc callback url: %w", err)
		}
		query = parsed.Query()
		// 有些浏览器会把 query 放在 fragment 里，兜底也解析一次。
		if len(query) == 0 && parsed.Fragment != "" {
			if fragQuery, fragErr := url.ParseQuery(strings.TrimPrefix(parsed.Fragment, "?")); fragErr == nil {
				query = fragQuery
			}
		}
	} else {
		parsed, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil {
			return nil, fmt.Errorf("parse siwc callback query: %w", err)
		}
		query = parsed
	}

	cb := &SIWCCallback{
		Code:             strings.TrimSpace(query.Get("code")),
		State:            strings.TrimSpace(query.Get("state")),
		ClientID:         strings.TrimSpace(query.Get("client_id")),
		Error:            strings.TrimSpace(query.Get("error")),
		ErrorDescription: strings.TrimSpace(query.Get("error_description")),
	}
	if cb.Error != "" {
		return cb, nil
	}
	for _, key := range []string{"code", "state", "client_id"} {
		if len(query[key]) > 1 {
			return nil, fmt.Errorf("siwc callback parameter %s appears more than once", key)
		}
	}
	if cb.Code == "" {
		return nil, errors.New("siwc callback is missing code")
	}
	if cb.State == "" {
		return nil, errors.New("siwc callback is missing state")
	}
	if cb.ClientID == "" {
		return nil, errors.New("siwc callback is missing client_id")
	}
	if !strings.HasPrefix(cb.ClientID, SIWCIssuedClientIDPrefix) {
		return nil, fmt.Errorf("siwc callback client_id %q is not an issued client id", cb.ClientID)
	}
	return cb, nil
}

// SplitScopes 把空白分隔的 scope 串拆成切片。
func SplitScopes(scope string) []string {
	return strings.Fields(scope)
}

// ValidateSIWCTokenResponse 校验 token 端点响应：
// access_token / refresh_token 非空、expires_in > 0、scope 含 chatgpt.tokens.use.direct；
// requireIDToken 为真时（首次换码）还必须有 id_token。
func ValidateSIWCTokenResponse(resp *TokenResponse, requireIDToken bool) error {
	if resp == nil {
		return errors.New("siwc token response is nil")
	}
	if strings.TrimSpace(resp.AccessToken) == "" {
		return errors.New("siwc token response has empty access_token")
	}
	if strings.TrimSpace(resp.RefreshToken) == "" {
		return errors.New("siwc token response has empty refresh_token")
	}
	if requireIDToken && strings.TrimSpace(resp.IDToken) == "" {
		return errors.New("siwc token response has empty id_token")
	}
	if resp.ExpiresIn <= 0 {
		return errors.New("siwc token response has invalid expires_in")
	}
	scopes := SplitScopes(resp.Scope)
	if len(scopes) == 0 {
		return errors.New("siwc token response has empty scope")
	}
	if !slices.Contains(scopes, SIWCRequiredScope) {
		return ErrSIWCScopeNotGranted
	}
	return nil
}
