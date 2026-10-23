package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestOpenAISIWCOAuthClient_ExchangeCode(t *testing.T) {
	var gotForm map[string]string
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		gotForm = map[string]string{}
		for k := range r.PostForm {
			gotForm[k] = r.PostForm.Get(k)
		}
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","id_token":"id","expires_in":3600,"scope":"` + openai.SIWCScope + `"}`))
	}))
	defer srv.Close()

	client := &openaiSIWCOAuthClient{tokenURL: srv.URL}
	resp, err := client.ExchangeCode(context.Background(), "code-1", "verifier-1", "", "oaiapp_abc", "")
	require.NoError(t, err)
	require.Equal(t, "at", resp.AccessToken)
	require.Equal(t, "rt", resp.RefreshToken)

	require.Equal(t, "authorization_code", gotForm["grant_type"])
	require.Equal(t, "oaiapp_abc", gotForm["client_id"])
	require.Equal(t, "code-1", gotForm["code"])
	require.Equal(t, "verifier-1", gotForm["code_verifier"])
	require.Equal(t, openai.SIWCDefaultRedirectURI, gotForm["redirect_uri"])
	require.Equal(t, openai.SIWCResource, gotForm["resource"])
	// 不做 Codex 伪装
	require.Empty(t, gotHeaders.Get("originator"))
	require.NotContains(t, strings.ToLower(gotHeaders.Get("User-Agent")), "codex")
	require.Equal(t, "application/json", gotHeaders.Get("Accept"))
}

func TestOpenAISIWCOAuthClient_Refresh(t *testing.T) {
	var gotForm map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		gotForm = map[string]string{}
		for k := range r.PostForm {
			gotForm[k] = r.PostForm.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at2","refresh_token":"rt2","expires_in":3600,"scope":"` + openai.SIWCScope + `"}`))
	}))
	defer srv.Close()

	client := &openaiSIWCOAuthClient{tokenURL: srv.URL}
	resp, err := client.Refresh(context.Background(), "oaiapp_abc", "rt-old", "")
	require.NoError(t, err)
	require.Equal(t, "at2", resp.AccessToken)
	require.Equal(t, "rt2", resp.RefreshToken)

	require.Equal(t, "refresh_token", gotForm["grant_type"])
	require.Equal(t, "oaiapp_abc", gotForm["client_id"])
	require.Equal(t, "rt-old", gotForm["refresh_token"])
	require.Equal(t, openai.SIWCResource, gotForm["resource"])
	_, hasScope := gotForm["scope"]
	require.False(t, hasScope, "SIWC 刷新不能带 scope")
}

func TestOpenAISIWCOAuthClient_ErrorsCarryUpstreamBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token_reused"}`))
	}))
	defer srv.Close()

	client := &openaiSIWCOAuthClient{tokenURL: srv.URL}
	_, err := client.Refresh(context.Background(), "oaiapp_abc", "rt-old", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "refresh_token_reused")
	require.Contains(t, err.Error(), "status 400")

	_, err = client.Refresh(context.Background(), "", "rt", "")
	require.Error(t, err)
	_, err = client.Refresh(context.Background(), "oaiapp_x", "", "")
	require.Error(t, err)
	_, err = client.ExchangeCode(context.Background(), "c", "v", "", "", "")
	require.Error(t, err)
}
