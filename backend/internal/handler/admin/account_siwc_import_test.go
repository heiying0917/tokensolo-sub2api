package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestNormalizeCodexImportEntry_SIWCToolkitFormat(t *testing.T) {
	expired := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	req := CodexSessionImportRequest{Content: `{
		"type":"chatgpt_siwc","email":"Alice@Example.com","client_id":"oaiapp_abc",
		"access_token":"at-old","refresh_token":"rt-1","id_token":"",
		"plan_type":"pro","issuer":"https://auth.openai.com","subject":"user-1",
		"scopes":["openid","profile","email","offline_access","resource.invoke","chatgpt.tokens.use.direct"],
		"expires_at":"` + expired + `","saved_at":"2026-10-20T00:00:00Z","authorization_profile":"openclaw-v1"
	}`}
	entries, err := parseCodexSessionImportEntries(req)
	if err != nil || len(entries) != 1 {
		t.Fatalf("parse entries: %v (%d)", err, len(entries))
	}
	item, err := normalizeCodexImportEntry(entries[0])
	if err != nil {
		t.Fatalf("normalize siwc: %v", err)
	}
	if !item.IsSIWC {
		t.Fatal("expected IsSIWC")
	}
	if item.Credentials["auth_mode"] != service.OpenAIAuthModeSIWC {
		t.Fatalf("auth_mode = %v", item.Credentials["auth_mode"])
	}
	if item.Credentials["client_id"] != "oaiapp_abc" || item.Credentials["refresh_token"] != "rt-1" {
		t.Fatalf("credentials = %v", item.Credentials)
	}
	if item.Credentials["email"] != "Alice@Example.com" || item.Credentials["plan_type"] != "pro" {
		t.Fatalf("email/plan = %v", item.Credentials)
	}
	if item.Credentials["siwc_issuer"] != "https://auth.openai.com" || item.Credentials["siwc_subject"] != "user-1" {
		t.Fatalf("issuer/subject = %v", item.Credentials)
	}
	if _, ok := item.Credentials["chatgpt_account_id"]; ok {
		t.Fatal("siwc must not carry chatgpt_account_id")
	}
	if item.Extra["import_source"] != service.OpenAISIWCImportSource || item.Extra["privacy_mode"] != service.OpenAIPrivacyModeSIWCNotApplicable {
		t.Fatalf("extra = %v", item.Extra)
	}
	if item.Extra["siwc_authorization_profile"] != "openclaw-v1" {
		t.Fatalf("extra profile = %v", item.Extra)
	}
	if len(item.IdentityKeys) != 1 || item.IdentityKeys[0] != "siwc:client_id:oaiapp_abc" {
		t.Fatalf("identity keys = %v", item.IdentityKeys)
	}
	// 过期 AT 不拒绝
	if item.TokenExpiresAt == nil || !item.TokenExpiresAt.Before(time.Now()) {
		t.Fatalf("expires_at should be kept as past: %v", item.TokenExpiresAt)
	}
	if !strings.Contains(item.Name, "Alice@Example.com") {
		t.Fatalf("name = %s", item.Name)
	}
}

func TestNormalizeCodexImportEntry_SIWCWrappedAndBareFormats(t *testing.T) {
	wrapped := `{"type":"codex","token_sharing":true,"siwc":{"client_id":"oaiapp_w","refresh_token":"rt-w","email":"w@x.com","access_token":"at"}}`
	bare := `{"client_id":"oaiapp_b","refresh_token":"rt-b","email":"b@x.com"}`
	entries, err := parseCodexSessionImportEntries(CodexSessionImportRequest{Contents: []string{wrapped, bare}})
	if err != nil || len(entries) != 2 {
		t.Fatalf("parse: %v (%d)", err, len(entries))
	}
	w, err := normalizeCodexImportEntry(entries[0])
	if err != nil || !w.IsSIWC || w.Credentials["client_id"] != "oaiapp_w" {
		t.Fatalf("wrapped: %v %+v", err, w)
	}
	b, err := normalizeCodexImportEntry(entries[1])
	if err != nil || !b.IsSIWC || b.Credentials["client_id"] != "oaiapp_b" {
		t.Fatalf("bare: %v %+v", err, b)
	}
	// 没有 access_token / expires_at：写占位并提示将立即刷新
	if b.Credentials["expires_at"] == "" {
		t.Fatal("expires_at placeholder expected")
	}
	if len(b.WarningTexts) == 0 {
		t.Fatal("expected warning about missing access_token")
	}
}

func TestNormalizeCodexImportEntry_SIWCValidation(t *testing.T) {
	cases := map[string]string{
		"缺 refresh_token":  `{"type":"chatgpt_siwc","client_id":"oaiapp_x","email":"a@x.com"}`,
		"缺 email":          `{"type":"chatgpt_siwc","client_id":"oaiapp_x","refresh_token":"rt"}`,
		"client_id 前缀错":    `{"type":"chatgpt_siwc","client_id":"app_x","refresh_token":"rt","email":"a@x.com"}`,
		"scope 缺 required": `{"type":"chatgpt_siwc","client_id":"oaiapp_x","refresh_token":"rt","email":"a@x.com","scopes":["openid"]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			entries, err := parseCodexSessionImportEntries(CodexSessionImportRequest{Content: content})
			if err != nil || len(entries) != 1 {
				t.Fatalf("parse: %v", err)
			}
			if _, err := normalizeCodexImportEntry(entries[0]); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestCodexAccountIndex_SIWCRowsKeyedByClientIDOnly(t *testing.T) {
	siwc1 := service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"auth_mode": "siwc", "client_id": "oaiapp_1", "email": "a@x.com", "access_token": "at1"}}
	siwc2 := service.Account{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"auth_mode": "siwc", "client_id": "oaiapp_2", "email": "a@x.com", "access_token": "at2"}}
	codex := service.Account{ID: 3, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"email": "a@x.com", "access_token": "at3", "chatgpt_account_id": "acc", "chatgpt_user_id": "u"}}
	index := buildCodexAccountIndex([]service.Account{siwc1, siwc2, codex})

	// 同 client_id 命中自己
	found, key := index.Find(siwcImportIdentityKeys("oaiapp_2"), "")
	if found == nil || found.ID != 2 || key != "siwc:client_id:oaiapp_2" {
		t.Fatalf("find oaiapp_2 = %v %q", found, key)
	}
	// 新 client_id 不与同邮箱的其它行匹配
	if found, _ := index.Find(siwcImportIdentityKeys("oaiapp_3"), ""); found != nil {
		t.Fatalf("new client_id must not match existing rows, got %d", found.ID)
	}
	// Codex 行按 account/user 匹配，不被 SIWC 行干扰
	found, _ = index.Find(buildCodexImportIdentityKeys("acc", "u", "a@x.com", "at3", "rt"), "u")
	if found == nil || found.ID != 3 {
		t.Fatalf("codex lookup = %v", found)
	}
}
