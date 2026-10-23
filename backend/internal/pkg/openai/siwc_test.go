package openai

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildSIWCAuthorizationURL(t *testing.T) {
	raw, err := BuildSIWCAuthorizationURL(SIWCAuthorizationParams{
		State:         "state-1",
		Nonce:         "nonce-1",
		CodeChallenge: "challenge-1",
		DeviceID:      "3F2504E0-4F89-11D3-9A0C-0305E82C3301",
		AgentName:     "my-agent",
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(raw, SIWCAuthorizeURL+"?"))

	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, SIWCDynamicClientID, q.Get("client_id"))
	require.Equal(t, "my-agent", q.Get("agent_name_hint"))
	require.Equal(t, "urn:uuid:3f2504e0-4f89-11d3-9a0c-0305e82c3301", q.Get("ext_agent_host_id"))
	require.Equal(t, "code", q.Get("response_type"))
	require.Equal(t, SIWCDefaultRedirectURI, q.Get("redirect_uri"))
	require.Equal(t, SIWCResource, q.Get("resource"))
	require.Equal(t, SIWCScope, q.Get("scope"))
	require.Equal(t, "state-1", q.Get("state"))
	require.Equal(t, "challenge-1", q.Get("code_challenge"))
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.Equal(t, "nonce-1", q.Get("nonce"))
	// Codex 专属参数不能出现
	require.Empty(t, q.Get("codex_cli_simplified_flow"))
	require.Empty(t, q.Get("id_token_add_organizations"))
}

func TestBuildSIWCAuthorizationURL_DefaultsAndValidation(t *testing.T) {
	raw, err := BuildSIWCAuthorizationURL(SIWCAuthorizationParams{State: "s", Nonce: "n", CodeChallenge: "c"})
	require.NoError(t, err)
	q, _ := url.Parse(raw)
	require.Equal(t, SIWCDefaultAgentName, q.Query().Get("agent_name_hint"))
	require.Empty(t, q.Query().Get("ext_agent_host_id"))

	_, err = BuildSIWCAuthorizationURL(SIWCAuthorizationParams{Nonce: "n", CodeChallenge: "c"})
	require.Error(t, err)
	_, err = BuildSIWCAuthorizationURL(SIWCAuthorizationParams{State: "s", CodeChallenge: "c"})
	require.Error(t, err)
	_, err = BuildSIWCAuthorizationURL(SIWCAuthorizationParams{State: "s", Nonce: "n"})
	require.Error(t, err)
	_, err = BuildSIWCAuthorizationURL(SIWCAuthorizationParams{State: "s", Nonce: "n", CodeChallenge: "c", DeviceID: "not-a-uuid"})
	require.Error(t, err)
}

func TestParseSIWCCallbackURL(t *testing.T) {
	t.Run("完整回调", func(t *testing.T) {
		cb, err := ParseSIWCCallbackURL("http://127.0.0.1:1455/auth/callback?code=abc&state=st&client_id=oaiapp_123")
		require.NoError(t, err)
		require.Equal(t, "abc", cb.Code)
		require.Equal(t, "st", cb.State)
		require.Equal(t, "oaiapp_123", cb.ClientID)
		require.Empty(t, cb.Error)
	})
	t.Run("裸 query", func(t *testing.T) {
		cb, err := ParseSIWCCallbackURL("?code=abc&state=st&client_id=oaiapp_123")
		require.NoError(t, err)
		require.Equal(t, "oaiapp_123", cb.ClientID)
	})
	t.Run("fragment 兜底", func(t *testing.T) {
		cb, err := ParseSIWCCallbackURL("http://127.0.0.1:1455/auth/callback#code=abc&state=st&client_id=oaiapp_1")
		require.NoError(t, err)
		require.Equal(t, "abc", cb.Code)
	})
	t.Run("上游错误原样返回", func(t *testing.T) {
		cb, err := ParseSIWCCallbackURL("http://127.0.0.1:1455/auth/callback?error=access_denied&error_description=nope&state=st")
		require.NoError(t, err)
		require.Equal(t, "access_denied", cb.Error)
		require.Equal(t, "nope", cb.ErrorDescription)
	})
	for name, raw := range map[string]string{
		"空":             "",
		"缺 code":        "http://127.0.0.1:1455/auth/callback?state=st&client_id=oaiapp_1",
		"缺 state":       "http://127.0.0.1:1455/auth/callback?code=c&client_id=oaiapp_1",
		"缺 client_id":   "http://127.0.0.1:1455/auth/callback?code=c&state=st",
		"client_id 前缀错": "http://127.0.0.1:1455/auth/callback?code=c&state=st&client_id=app_xxx",
		"重复参数":          "http://127.0.0.1:1455/auth/callback?code=c&code=d&state=st&client_id=oaiapp_1",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSIWCCallbackURL(raw)
			require.Error(t, err)
		})
	}
}

func TestValidateSIWCTokenResponse(t *testing.T) {
	good := func() *TokenResponse {
		return &TokenResponse{AccessToken: "at", RefreshToken: "rt", IDToken: "id", ExpiresIn: 3600, Scope: SIWCScope}
	}
	require.NoError(t, ValidateSIWCTokenResponse(good(), true))

	r := good()
	r.IDToken = ""
	require.Error(t, ValidateSIWCTokenResponse(r, true))
	require.NoError(t, ValidateSIWCTokenResponse(r, false))

	r = good()
	r.Scope = "openid profile email offline_access"
	err := ValidateSIWCTokenResponse(r, true)
	require.ErrorIs(t, err, ErrSIWCScopeNotGranted)

	r = good()
	r.ExpiresIn = 0
	require.Error(t, ValidateSIWCTokenResponse(r, true))

	r = good()
	r.RefreshToken = ""
	require.Error(t, ValidateSIWCTokenResponse(r, false))

	require.Error(t, ValidateSIWCTokenResponse(nil, false))
}

func TestGenerateRandomBase64URL(t *testing.T) {
	v, err := GenerateRandomBase64URL()
	require.NoError(t, err)
	require.Len(t, v, 43) // 32 字节 base64url 无填充
	require.NotContains(t, v, "=")
	require.NotContains(t, v, "+")
	require.NotContains(t, v, "/")
}
