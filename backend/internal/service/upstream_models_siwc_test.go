//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// SIWC 账号同步上游模型：借同邮箱 Codex 管理凭据拉 codex/models，并过滤隐藏模型与 SIWC 拒绝的 gpt-5.5 家族。
func TestSyncUpstreamModelCatalog_SIWCUsesManagementCredential(t *testing.T) {
	siwc := siwcTestAccount(21, "a@b.c")
	mgmt := codexTestAccount(22, "a@b.c")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"models":[{"slug":"gpt-6-astra","visibility":"list"},{"slug":"gpt-5.5"},{"slug":"gpt-5.5-codex"},{"slug":"gpt-reserve","visibility":"hide"},{"slug":"codex-auto-review","visibility":"hide"},{"slug":"gpt-6-luna","visibility":"list"}]}`)),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		accountRepo:  newSIWCTestRepo(siwc, mgmt),
		cfg:          upstreamModelSyncTestConfig(),
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &siwc)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-6-astra", "gpt-6-luna"}, models)
	require.Equal(t, chatgptCodexModelsURL, upstream.lastReq.URL.Scheme+"://"+upstream.lastReq.URL.Host+upstream.lastReq.URL.Path)
	require.Equal(t, "Bearer codex-at", upstream.lastReq.Header.Get("Authorization"), "必须用管理凭据令牌，SIWC 令牌打 chatgpt.com 会 401")
}

func TestSyncUpstreamModelCatalog_SIWCWithoutManagementCredentialFails(t *testing.T) {
	siwc := siwcTestAccount(31, "x@y.z")
	upstream := &httpUpstreamRecorder{}
	svc := &AccountTestService{
		httpUpstream: upstream,
		accountRepo:  newSIWCTestRepo(siwc),
		cfg:          upstreamModelSyncTestConfig(),
	}

	_, err := svc.FetchUpstreamSupportedModels(context.Background(), &siwc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "management credential")
	require.Nil(t, upstream.lastReq, "没有管理凭据时不得向上游发请求")
}
