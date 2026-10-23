//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newSIWCForwardTestContext(t *testing.T, path string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.99.0 (Mac OS 15; arm64)")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set("session_id", "client-session")
	c.Request.Header.Set("x-codex-turn-state", "blob")
	c.Request.Header.Set("Accept-Language", "zh-CN")
	c.Request.Header.Set("Authorization", "Bearer inbound-key")
	c.Set("api_key_id", int64(42))
	return c
}

func siwcForwardTestAccount() *Account {
	acc := siwcRefreshTestAccount(100)
	acc.Credentials["email"] = "a@x.com"
	return acc
}

func TestAccount_UsesOpenAICodexProtocol_ExcludesSIWC(t *testing.T) {
	require.False(t, siwcForwardTestAccount().UsesOpenAICodexProtocol())
	codex := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.True(t, codex.UsesOpenAICodexProtocol())
	setup := &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}
	require.True(t, setup.UsesOpenAICodexProtocol())
	require.True(t, siwcForwardTestAccount().IsOpenAIOAuthLike(), "SIWC 仍是 OAuth-like，429/调度语义沿用")
}

func TestBuildOpenAISIWCUpstreamRequest_Headers(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	c := newSIWCForwardTestContext(t, "/v1/responses")
	account := siwcForwardTestAccount()
	body := []byte(`{"model":"gpt-6-astra","input":"hi","store":true,"max_output_tokens":100,"temperature":0.2,"prompt_cache_key":"pck-1"}`)

	req, err := svc.buildOpenAISIWCUpstreamRequest(context.Background(), c, account, body, "siwc-at", "pck-1")
	require.NoError(t, err)

	require.Equal(t, "https://api.openai.com/v1/responses", req.URL.String())
	require.Equal(t, "api.openai.com", req.Host, "Host 必须是 api.openai.com 而不是 chatgpt.com")
	require.Equal(t, "Bearer siwc-at", req.Header.Get("Authorization"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, "text/event-stream", req.Header.Get("Accept"))
	require.Equal(t, "zh-CN", req.Header.Get("Accept-Language"))

	// 不发任何 Codex 账号头 / 指纹头，也不透传客户端的 session / turn-state
	for _, h := range []string{"chatgpt-account-id", "x-codex-turn-state", "x-codex-installation-id", "x-codex-beta-features", "version", "conversation_id", "x-openai-fedramp"} {
		require.Empty(t, req.Header.Get(h), h)
	}
	require.NotEqual(t, "client-session", req.Header.Get("session_id"))

	// 三个会话头同值且稳定
	sid := req.Header.Get("session_id")
	require.Len(t, sid, openAISIWCSessionHashLen)
	require.Equal(t, sid, req.Header.Get("Thread-Id"))
	require.Equal(t, sid, req.Header.Get("X-Client-Request-Id"))
	req2, err := svc.buildOpenAISIWCUpstreamRequest(context.Background(), c, account, body, "siwc-at", "pck-1")
	require.NoError(t, err)
	require.Equal(t, sid, req2.Header.Get("session_id"))

	// 默认 codex 身份模式：带规范 UA 与 originator，但不带账号头
	ua, originator := CodexCanonicalAuthIdentity()
	require.Equal(t, ua, req.Header.Get("User-Agent"))
	require.Equal(t, originator, req.Header.Get("originator"))

	// profile 为 OpenAI
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))

	// 请求体已裁剪
	sent, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, false, gjson.GetBytes(sent, "store").Bool())
	require.Equal(t, true, gjson.GetBytes(sent, "stream").Bool())
	require.False(t, gjson.GetBytes(sent, "max_output_tokens").Exists())
	require.False(t, gjson.GetBytes(sent, "temperature").Exists())
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(sent, "model").String())
}

func TestBuildOpenAISIWCUpstreamRequest_NeutralIdentityAndCustomUA(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	c := newSIWCForwardTestContext(t, "/v1/responses")
	account := siwcForwardTestAccount()
	account.Extra = map[string]any{openAISIWCIdentityModeExtraKey: "neutral"}

	req, err := svc.buildOpenAISIWCUpstreamRequest(context.Background(), c, account, []byte(`{"model":"gpt-6-luna","input":"hi"}`), "siwc-at", "")
	require.NoError(t, err)
	require.Equal(t, openAISIWCNeutralUserAgent, req.Header.Get("User-Agent"))
	require.Empty(t, req.Header.Get("originator"))
	// 没有 prompt_cache_key：每个请求独立会话
	require.NotEmpty(t, req.Header.Get("session_id"))
	req2, _ := svc.buildOpenAISIWCUpstreamRequest(context.Background(), c, account, []byte(`{"model":"gpt-6-luna","input":"hi"}`), "siwc-at", "")
	require.NotEqual(t, req.Header.Get("session_id"), req2.Header.Get("session_id"))

	account.Credentials["user_agent"] = "MyAgent/1.0"
	req3, err := svc.buildOpenAISIWCUpstreamRequest(context.Background(), c, account, []byte(`{"model":"gpt-6-luna","input":"hi"}`), "siwc-at", "")
	require.NoError(t, err)
	require.Equal(t, "MyAgent/1.0", req3.Header.Get("User-Agent"))
}

func TestBuildOpenAISIWCUpstreamRequest_Guards(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	c := newSIWCForwardTestContext(t, "/v1/responses")
	_, err := svc.buildOpenAISIWCUpstreamRequest(context.Background(), c, siwcForwardTestAccount(), []byte(`{}`), "", "")
	require.Error(t, err, "空 token 必须拒绝")
	codex := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	_, err = svc.buildOpenAISIWCUpstreamRequest(context.Background(), c, codex, []byte(`{}`), "at", "")
	require.Error(t, err)

	// compact 路径直接拒绝
	compact := newSIWCForwardTestContext(t, "/v1/responses/compact")
	_, err = svc.buildOpenAISIWCUpstreamRequest(context.Background(), compact, siwcForwardTestAccount(), []byte(`{}`), "at", "")
	require.True(t, IsOpenAISIWCUnsupportedCapabilityError(err))
}

func TestNormalizeOpenAISIWCRequestBody(t *testing.T) {
	body := []byte(`{
		"model":"gpt-6-astra","input":"hi","store":true,"stream":false,
		"max_output_tokens":10,"max_completion_tokens":10,"max_tokens":10,"temperature":0.1,
		"prompt_cache_retention":"24h","prompt_cache_options":{},"previous_response_id":"resp_1","safety_identifier":"u1",
		"tools":[
			{"type":"function","name":"lookup"},
			{"type":"web_search"},
			{"type":"image_generation"},
			{"type":"custom","name":"c1"},
			{"type":"namespace","name":"ns"},
			{"type":"file_search"}
		],
		"tool_choice":{"type":"image_generation"}
	}`)
	out, err := normalizeOpenAISIWCRequestBody(body)
	require.NoError(t, err)
	require.Equal(t, false, gjson.GetBytes(out, "store").Bool())
	require.Equal(t, true, gjson.GetBytes(out, "stream").Bool())
	for _, field := range openAISIWCStrippedFields {
		require.False(t, gjson.GetBytes(out, field).Exists(), field)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	require.Len(t, tools, 4)
	types := map[string]bool{}
	for _, tool := range tools {
		types[tool.Get("type").String()] = true
	}
	require.True(t, types["function"] && types["web_search"] && types["custom"] && types["namespace"])
	require.False(t, types["image_generation"] || types["file_search"])
	require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice").String(), "指向被剔除工具的 tool_choice 回落 auto")
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(out, "model").String())

	// 全部工具被剔除时连 tools / tool_choice 一起删
	out, err = normalizeOpenAISIWCRequestBody([]byte(`{"model":"m","tools":[{"type":"image_generation"}],"tool_choice":"required"}`))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "tools").Exists())
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())

	// 指向保留工具的 tool_choice 不动
	out, err = normalizeOpenAISIWCRequestBody([]byte(`{"model":"m","tools":[{"type":"function","name":"f"}],"tool_choice":{"type":"function","name":"f"}}`))
	require.NoError(t, err)
	require.Equal(t, "function", gjson.GetBytes(out, "tool_choice.type").String())

	// 空 body 原样
	out, err = normalizeOpenAISIWCRequestBody(nil)
	require.NoError(t, err)
	require.Nil(t, out)

	_, err = normalizeOpenAISIWCRequestBody([]byte(`not json`))
	require.Error(t, err)
}

func TestDetectOpenAISIWCUnsupportedCapability(t *testing.T) {
	c := newSIWCForwardTestContext(t, "/v1/responses")
	require.NoError(t, detectOpenAISIWCUnsupportedCapability(c, []byte(`{"input":[{"type":"message","role":"user","content":"hi"}]}`)))
	require.NoError(t, detectOpenAISIWCUnsupportedCapability(c, []byte(`{"input":"plain string"}`)))

	err := detectOpenAISIWCUnsupportedCapability(c, []byte(`{"input":[{"type":"compaction_trigger"}]}`))
	require.True(t, IsOpenAISIWCUnsupportedCapabilityError(err))
	require.Contains(t, err.Error(), "compaction_trigger")

	err = detectOpenAISIWCUnsupportedCapability(c, []byte(`{"input":[{"type":"message"},{"type":"tool_search_call"}]}`))
	require.True(t, IsOpenAISIWCUnsupportedCapabilityError(err))

	compact := newSIWCForwardTestContext(t, "/v1/responses/compact")
	err = detectOpenAISIWCUnsupportedCapability(compact, []byte(`{}`))
	require.True(t, IsOpenAISIWCUnsupportedCapabilityError(err))
}

func TestOpenAIWSProtocolResolver_SIWCForcesHTTP(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	resolver := NewOpenAIWSProtocolResolver(cfg)

	decision := resolver.Resolve(siwcForwardTestAccount())
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, decision.Transport)
	require.Equal(t, "siwc_http_only", decision.Reason)
}

func TestOpenAISIWCSessionID(t *testing.T) {
	acc := siwcForwardTestAccount()
	a := openAISIWCSessionID(1, acc, "k")
	b := openAISIWCSessionID(1, acc, "k")
	require.Equal(t, a, b)
	require.NotEqual(t, a, openAISIWCSessionID(2, acc, "k"), "不同 API Key 隔离")
	other := siwcForwardTestAccount()
	other.Credentials["client_id"] = "oaiapp_other"
	require.NotEqual(t, a, openAISIWCSessionID(1, other, "k"), "不同凭据隔离")
	require.NotEqual(t, openAISIWCSessionID(1, acc, ""), openAISIWCSessionID(1, acc, ""), "无 cache key 不绑定会话")
}

func TestNormalizeOpenAISIWCRequestBody_StringInputBecomesList(t *testing.T) {
	out, err := normalizeOpenAISIWCRequestBody([]byte(`{"model":"gpt-6-luna","input":"你好"}`))
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(out, "input").IsArray())
	require.Equal(t, "user", gjson.GetBytes(out, "input.0.role").String())
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.0.content.0.type").String())
	require.Equal(t, "你好", gjson.GetBytes(out, "input.0.content.0.text").String())

	// 已是数组则原样保留
	out, err = normalizeOpenAISIWCRequestBody([]byte(`{"model":"m","input":[{"role":"user","content":"x"}]}`))
	require.NoError(t, err)
	require.Equal(t, "x", gjson.GetBytes(out, "input.0.content").String())
}

func TestNormalizeOpenAISIWCRequestBody_FoldsConfigurationUpdate(t *testing.T) {
	// 与 cpa e2f3f0c 的测试载荷一致：取最后一条强度，保留 summary，删除全部该项
	body := []byte(`{"model":"gpt-6-sol","reasoning":{"effort":"medium","summary":"auto"},"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},` +
		`{"type":"configuration_update","reasoning":{"effort":"low"}},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply"}]},` +
		`{"type":"configuration_update","reasoning":{"effort":"high"}},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}]}`)
	out, err := normalizeOpenAISIWCRequestBody(body)
	require.NoError(t, err)
	require.Equal(t, "high", gjson.GetBytes(out, "reasoning.effort").String())
	require.Equal(t, "auto", gjson.GetBytes(out, "reasoning.summary").String())
	input := gjson.GetBytes(out, "input").Array()
	require.Len(t, input, 3)
	for _, item := range input {
		require.NotEqual(t, "configuration_update", item.Get("type").String())
	}
	require.Equal(t, "output_text", gjson.GetBytes(out, "input.1.content.0.type").String(), "assistant 消息保持 output_text")

	// auto / 未知档位：只删项，顶层不动
	out, err = normalizeOpenAISIWCRequestBody([]byte(`{"model":"m","reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"auto"}},{"type":"message","role":"user","content":"x"}]}`))
	require.NoError(t, err)
	require.Equal(t, "medium", gjson.GetBytes(out, "reasoning.effort").String())
	require.Len(t, gjson.GetBytes(out, "input").Array(), 1)

	// 原请求没有 reasoning 时新建
	out, err = normalizeOpenAISIWCRequestBody([]byte(`{"model":"m","input":[{"type":"configuration_update","reasoning":{"effort":"XHigh"}}]}`))
	require.NoError(t, err)
	require.Equal(t, "xhigh", gjson.GetBytes(out, "reasoning.effort").String())
	require.Len(t, gjson.GetBytes(out, "input").Array(), 0)
}

func TestNormalizeOpenAISIWCRequestBody_RewritesAgentMessages(t *testing.T) {
	body := []byte(`{"model":"gpt-6-sol","input":[` +
		`{"type":"agent_message","author":"sub-1","recipient":"main","internal_chat_message_metadata_passthrough":{"a":1},` +
		`"content":[{"type":"encrypted_content","encrypted_content":"payload"},{"type":"output_text","text":"done"}]},` +
		`{"type":"message","role":"user","author":"x","content":[{"type":"output_text","text":"echo"}]}]}`)
	out, err := normalizeOpenAISIWCRequestBody(body)
	require.NoError(t, err)
	require.Equal(t, "message", gjson.GetBytes(out, "input.0.type").String())
	require.Equal(t, "user", gjson.GetBytes(out, "input.0.role").String())
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.0.content.0.type").String())
	require.Equal(t, "payload", gjson.GetBytes(out, "input.0.content.0.text").String())
	require.False(t, gjson.GetBytes(out, "input.0.content.0.encrypted_content").Exists())
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.0.content.1.type").String())
	for _, field := range []string{"author", "recipient", "internal_chat_message_metadata_passthrough"} {
		require.False(t, gjson.GetBytes(out, "input.0."+field).Exists(), field)
	}
	require.False(t, gjson.GetBytes(out, "input.1.author").Exists())
	require.Equal(t, "input_text", gjson.GetBytes(out, "input.1.content.0.type").String())
}
