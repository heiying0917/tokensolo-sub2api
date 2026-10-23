package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type siwcOAuthClientStub struct {
	exchangeCalls int32
	refreshCalls  int32
	lastClientID  string
	lastRefreshRT string
	lastCtxErr    error
	exchangeResp  *openai.TokenResponse
	refreshResp   *openai.TokenResponse
	refreshErr    error
	nonce         string
	email         string
}

func fakeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func (s *siwcOAuthClientStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, clientID, proxyURL string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.exchangeCalls, 1)
	s.lastClientID = clientID
	if s.exchangeResp != nil {
		return s.exchangeResp, nil
	}
	return &openai.TokenResponse{
		AccessToken:  "siwc-at",
		RefreshToken: "siwc-rt",
		IDToken:      "", // 由测试在 exchangeResp 里提供
		ExpiresIn:    3600,
		Scope:        openai.SIWCScope,
	}, nil
}

func (s *siwcOAuthClientStub) Refresh(ctx context.Context, clientID, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.refreshCalls, 1)
	s.lastClientID = clientID
	s.lastRefreshRT = refreshToken
	s.lastCtxErr = ctx.Err()
	if s.refreshErr != nil {
		return nil, s.refreshErr
	}
	if s.refreshResp != nil {
		return s.refreshResp, nil
	}
	return &openai.TokenResponse{AccessToken: "siwc-at-2", RefreshToken: "siwc-rt-2", ExpiresIn: 3600, Scope: openai.SIWCScope}, nil
}

func newSIWCServiceForTest(t *testing.T, client *siwcOAuthClientStub, repo AccountRepository) (*OpenAISIWCOAuthService, *OpenAISIWCAuthURLResult, *openai.OAuthSession) {
	t.Helper()
	svc := NewOpenAISIWCOAuthService(nil, client, nil, repo)
	result, err := svc.GenerateAuthURL(context.Background(), &OpenAISIWCGenerateAuthURLInput{AgentName: "unit"})
	require.NoError(t, err)
	session, err := svc.sessionStore.Get(context.Background(), result.SessionID)
	require.NoError(t, err)
	return svc, result, session
}

func TestOpenAISIWCOAuthService_GenerateAuthURL(t *testing.T) {
	svc, result, session := newSIWCServiceForTest(t, &siwcOAuthClientStub{}, nil)
	require.NotEmpty(t, result.AuthURL)
	require.NotEmpty(t, result.SessionID)
	require.Equal(t, session.State, result.State)
	require.Equal(t, int64(openai.SessionTTL/time.Second), result.ExpiresIn)

	require.Equal(t, openai.SIWCAuthModeSession, session.AuthMode)
	require.NotEmpty(t, session.Nonce)
	require.NotEmpty(t, session.CodeVerifier)
	require.True(t, openai.IsValidUUID(session.DeviceID))
	require.Equal(t, "unit", session.AgentName)
	require.Equal(t, openai.SIWCDefaultRedirectURI, session.RedirectURI)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, openai.SIWCDynamicClientID, q.Get("client_id"))
	require.Equal(t, session.State, q.Get("state"))
	require.Equal(t, session.Nonce, q.Get("nonce"))
	require.Equal(t, openai.GenerateCodeChallenge(session.CodeVerifier), q.Get("code_challenge"))
	require.Equal(t, "urn:uuid:"+session.DeviceID, q.Get("ext_agent_host_id"))
	_ = svc
}

func TestOpenAISIWCOAuthService_ExchangeCallback_Success(t *testing.T) {
	client := &siwcOAuthClientStub{}
	svc, result, session := newSIWCServiceForTest(t, client, nil)
	client.exchangeResp = &openai.TokenResponse{
		AccessToken:  "siwc-at",
		RefreshToken: "siwc-rt",
		IDToken: fakeIDToken(t, map[string]any{
			"sub":   "user-123",
			"email": "Alice@Example.com",
			"iss":   "https://auth.openai.com",
			"nonce": session.Nonce,
			"exp":   time.Now().Add(time.Hour).Unix(),
		}),
		ExpiresIn: 3600,
		Scope:     openai.SIWCScope,
	}

	info, err := svc.ExchangeCallback(context.Background(), &OpenAISIWCExchangeInput{
		SessionID:   result.SessionID,
		CallbackURL: "http://127.0.0.1:1455/auth/callback?code=c1&state=" + url.QueryEscape(session.State) + "&client_id=oaiapp_issued",
	})
	require.NoError(t, err)
	require.Equal(t, "oaiapp_issued", client.lastClientID)
	require.Equal(t, "oaiapp_issued", info.ClientID)
	require.Equal(t, "Alice@Example.com", info.Email)
	require.Equal(t, "user-123", info.Subject)
	require.Equal(t, session.DeviceID, info.DeviceID)
	require.Equal(t, "unit", info.AgentName)
	require.Contains(t, info.Scopes, openai.SIWCRequiredScope)
	require.Greater(t, info.ExpiresAt, time.Now().Unix())

	// 会话用完即删
	_, err = svc.sessionStore.Get(context.Background(), result.SessionID)
	require.ErrorIs(t, err, ErrOpenAIOAuthSessionNotFound)

	creds := svc.BuildAccountCredentials(info)
	require.Equal(t, OpenAIAuthModeSIWC, creds["auth_mode"])
	require.Equal(t, "oaiapp_issued", creds["client_id"])
	require.Equal(t, "siwc-at", creds["access_token"])
	require.Equal(t, "siwc-rt", creds["refresh_token"])
	require.Equal(t, "Alice@Example.com", creds["email"])
	require.Equal(t, session.DeviceID, creds[openAISIWCDeviceIDCredentialKey])
	require.NotEmpty(t, creds["expires_at"])
	require.NotContains(t, creds, "chatgpt_account_id")
}

func TestOpenAISIWCOAuthService_ExchangeCallback_Failures(t *testing.T) {
	t.Run("state 不匹配", func(t *testing.T) {
		client := &siwcOAuthClientStub{}
		svc, result, _ := newSIWCServiceForTest(t, client, nil)
		_, err := svc.ExchangeCallback(context.Background(), &OpenAISIWCExchangeInput{
			SessionID:   result.SessionID,
			CallbackURL: "?code=c&state=wrong&client_id=oaiapp_x",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid oauth state")
		require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalls))
	})

	t.Run("上游拒绝授权", func(t *testing.T) {
		client := &siwcOAuthClientStub{}
		svc, result, _ := newSIWCServiceForTest(t, client, nil)
		_, err := svc.ExchangeCallback(context.Background(), &OpenAISIWCExchangeInput{
			SessionID:   result.SessionID,
			CallbackURL: "?error=access_denied&error_description=user+cancelled",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "access_denied")
		require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalls))
	})

	t.Run("scope 未授予", func(t *testing.T) {
		client := &siwcOAuthClientStub{}
		svc, result, session := newSIWCServiceForTest(t, client, nil)
		client.exchangeResp = &openai.TokenResponse{AccessToken: "a", RefreshToken: "r", IDToken: "x.y.z", ExpiresIn: 3600, Scope: "openid profile email offline_access"}
		_, err := svc.ExchangeCallback(context.Background(), &OpenAISIWCExchangeInput{
			SessionID:   result.SessionID,
			CallbackURL: "?code=c&state=" + url.QueryEscape(session.State) + "&client_id=oaiapp_x",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "token sharing")
	})

	t.Run("nonce 不匹配", func(t *testing.T) {
		client := &siwcOAuthClientStub{}
		svc, result, session := newSIWCServiceForTest(t, client, nil)
		client.exchangeResp = &openai.TokenResponse{
			AccessToken: "a", RefreshToken: "r", ExpiresIn: 3600, Scope: openai.SIWCScope,
			IDToken: fakeIDToken(t, map[string]any{"sub": "u", "email": "a@x.com", "nonce": "other", "exp": time.Now().Add(time.Hour).Unix()}),
		}
		_, err := svc.ExchangeCallback(context.Background(), &OpenAISIWCExchangeInput{
			SessionID:   result.SessionID,
			CallbackURL: "?code=c&state=" + url.QueryEscape(session.State) + "&client_id=oaiapp_x",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "nonce")
	})

	t.Run("Codex 会话不能走 SIWC 换码", func(t *testing.T) {
		client := &siwcOAuthClientStub{}
		svc := NewOpenAISIWCOAuthService(nil, client, nil, nil)
		require.NoError(t, svc.sessionStore.Save(context.Background(), "codex-sid", &openai.OAuthSession{State: "s", CreatedAt: time.Now()}, time.Minute))
		_, err := svc.ExchangeCallback(context.Background(), &OpenAISIWCExchangeInput{SessionID: "codex-sid", CallbackURL: "?code=c&state=s&client_id=oaiapp_x"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "different oauth flow")
	})

	t.Run("会话不存在", func(t *testing.T) {
		svc := NewOpenAISIWCOAuthService(nil, &siwcOAuthClientStub{}, nil, nil)
		_, err := svc.ExchangeCallback(context.Background(), &OpenAISIWCExchangeInput{SessionID: "nope", CallbackURL: "?code=c&state=s&client_id=oaiapp_x"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "session not found")
	})
}

func TestOpenAISIWCOAuthService_RefreshCredential(t *testing.T) {
	client := &siwcOAuthClientStub{}
	svc := NewOpenAISIWCOAuthService(nil, client, nil, nil)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"auth_mode": "siwc", "client_id": "oaiapp_7", "refresh_token": "rt-old", "access_token": "at-old"},
	}

	// 请求 ctx 已取消，刷新仍必须发出（脱离请求 ctx）
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	creds, err := svc.RefreshCredential(ctx, account)
	require.NoError(t, err)
	require.NoError(t, client.lastCtxErr, "刷新调用的 ctx 不应继承取消")
	require.Equal(t, "oaiapp_7", client.lastClientID)
	require.Equal(t, "rt-old", client.lastRefreshRT)
	require.Equal(t, "siwc-at-2", creds["access_token"])
	require.Equal(t, "siwc-rt-2", creds["refresh_token"])
	require.NotEmpty(t, creds["expires_at"])
	require.NotContains(t, creds, "auth_mode", "增量键不应覆盖 auth_mode")

	// 上游不返回新 RT 时保留旧值（不写 refresh_token 键）
	client.refreshResp = &openai.TokenResponse{AccessToken: "at3", RefreshToken: "", ExpiresIn: 3600, Scope: openai.SIWCScope}
	_, err = svc.RefreshCredential(context.Background(), account)
	require.Error(t, err, "缺 refresh_token 的响应应被校验拒绝")

	client.refreshResp = nil
	client.refreshErr = errors.New("status 400, body: refresh_token_reused")
	_, err = svc.RefreshCredential(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refresh_token_reused")

	// 缺 client_id / refresh_token
	bad := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": "siwc", "refresh_token": "x"}}
	_, err = svc.RefreshCredential(context.Background(), bad)
	require.Error(t, err)
	bad.Credentials = map[string]any{"auth_mode": "siwc", "client_id": "oaiapp_1"}
	_, err = svc.RefreshCredential(context.Background(), bad)
	require.Error(t, err)

	// 非 SIWC 账号拒绝
	_, err = svc.RefreshCredential(context.Background(), &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "x"}})
	require.Error(t, err)
}

func TestBuildOpenAISIWCAccountExtra(t *testing.T) {
	extra := BuildOpenAISIWCAccountExtra(0)
	require.Equal(t, OpenAISIWCImportSource, extra["import_source"])
	require.Equal(t, OpenAIPrivacyModeSIWCNotApplicable, extra["privacy_mode"])
	require.Equal(t, true, extra["openai_ws_force_http"])
	require.NotContains(t, extra, openAISIWCManagementAccountExtraKey)
	require.True(t, shouldSkipOpenAIPrivacyEnsure(extra))

	extra = BuildOpenAISIWCAccountExtra(42)
	require.Equal(t, int64(42), extra[openAISIWCManagementAccountExtraKey])
}
