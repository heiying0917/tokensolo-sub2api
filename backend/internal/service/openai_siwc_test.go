//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
)

// siwcTestAccountRepo 复用 gemini 测试桩，并把 ListByPlatform 指向内存账号列表。
type siwcTestAccountRepo struct {
	mockAccountRepoForGemini
	listErr error
}

func (r *siwcTestAccountRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]Account, 0, len(r.accounts))
	for _, acc := range r.accounts {
		if acc.Platform == platform {
			out = append(out, acc)
		}
	}
	return out, nil
}

func newSIWCTestRepo(accounts ...Account) *siwcTestAccountRepo {
	repo := &siwcTestAccountRepo{}
	repo.accounts = accounts
	repo.accountsByID = make(map[int64]*Account, len(accounts))
	for i := range accounts {
		repo.accountsByID[accounts[i].ID] = &accounts[i]
	}
	return repo
}

func siwcTestAccount(id int64, email string) Account {
	return Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"auth_mode":     OpenAIAuthModeSIWC,
			"client_id":     "oaiapp_test",
			"access_token":  "siwc-at",
			"refresh_token": "siwc-rt",
			"email":         email,
		},
	}
}

func codexTestAccount(id int64, email string) Account {
	return Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":  "codex-at",
			"refresh_token": "codex-rt",
			"email":         email,
		},
	}
}

func TestAccount_IsOpenAISIWC(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil", account: nil, want: false},
		{name: "siwc", account: func() *Account { a := siwcTestAccount(1, "a@x.com"); return &a }(), want: true},
		{name: "siwc 大小写", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": " SIWC "}}, want: true},
		{name: "codex oauth", account: func() *Account { a := codexTestAccount(1, "a@x.com"); return &a }(), want: false},
		{name: "非 oauth 类型", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"auth_mode": "siwc"}}, want: false},
		{name: "非 openai 平台", account: &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": "siwc"}}, want: false},
		{name: "pat", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.IsOpenAISIWC(); got != tt.want {
				t.Fatalf("IsOpenAISIWC() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsOpenAISIWCServableModel(t *testing.T) {
	tests := map[string]bool{
		"":                    true,
		"gpt-6-astra":         true,
		"GPT-6-Luna":          true,
		"gpt-6.1-sol":         true,
		"gpt-5.6-terra":       true,
		"gpt-6-astra[1m]":     true,
		"openai/gpt-6-sol":    true,
		"gpt-5.5":             false,
		"gpt-5.5-codex":       false,
		"gpt-4o":              false,
		"claude-sonnet-4":     false,
		"deepseek-flash":      false,
		"gpt-6-astra-preview": false,
		"gpt-6":               false,
	}
	for model, want := range tests {
		t.Run(model, func(t *testing.T) {
			if got := isOpenAISIWCServableModel(model); got != want {
				t.Fatalf("isOpenAISIWCServableModel(%q) = %v, want %v", model, got, want)
			}
		})
	}
}

func TestAccount_IsModelSupported_SIWCUsesWhitelist(t *testing.T) {
	acc := siwcTestAccount(1, "a@x.com")
	if !acc.IsModelSupported("gpt-6-astra") {
		t.Fatal("siwc 账号应支持 gpt-6-astra")
	}
	if acc.IsModelSupported("gpt-4o") {
		t.Fatal("siwc 账号不应支持 gpt-4o")
	}
	// 显式 model_mapping 优先于白名单
	acc.Credentials["model_mapping"] = map[string]any{"my-alias": "gpt-6-astra"}
	if !acc.IsModelSupported("my-alias") {
		t.Fatal("显式映射应放行 my-alias")
	}
}

func TestAccount_SIWCCapabilityGates(t *testing.T) {
	acc := siwcTestAccount(1, "a@x.com")
	acc.Extra = map[string]any{"openai_passthrough": true}
	if acc.IsOpenAIPassthroughEnabled() {
		t.Fatal("siwc 账号不应允许透传")
	}
	if acc.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive) {
		t.Fatal("siwc 账号不应支持 Live")
	}
	if !acc.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) {
		t.Fatal("siwc 账号应支持 Responses")
	}
}

func TestResolveOpenAISIWCManagementAccount(t *testing.T) {
	ctx := context.Background()

	t.Run("显式指定优先", func(t *testing.T) {
		siwc := siwcTestAccount(1, "a@x.com")
		siwc.Extra = map[string]any{openAISIWCManagementAccountExtraKey: float64(20)}
		byEmail := codexTestAccount(10, "a@x.com")
		explicit := codexTestAccount(20, "other@x.com")
		repo := newSIWCTestRepo(siwc, byEmail, explicit)

		got, err := resolveOpenAISIWCManagementAccount(ctx, repo, &siwc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != 20 {
			t.Fatalf("want explicit account 20, got %d", got.ID)
		}
	})

	t.Run("显式指定但不可用则报缺失", func(t *testing.T) {
		siwc := siwcTestAccount(1, "a@x.com")
		siwc.Extra = map[string]any{openAISIWCManagementAccountExtraKey: int64(2)}
		other := siwcTestAccount(2, "a@x.com") // 另一份 SIWC 不能当管理凭据
		repo := newSIWCTestRepo(siwc, other)

		_, err := resolveOpenAISIWCManagementAccount(ctx, repo, &siwc)
		if !errors.Is(err, errOpenAISIWCManagementAccountMissing) {
			t.Fatalf("want errOpenAISIWCManagementAccountMissing, got %v", err)
		}
	})

	t.Run("按邮箱自动匹配取 ID 最小", func(t *testing.T) {
		siwc := siwcTestAccount(1, "A@X.com")
		older := codexTestAccount(5, "a@x.com")
		newer := codexTestAccount(7, "a@x.com")
		pat := codexTestAccount(3, "a@x.com")
		pat.Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken
		disabled := codexTestAccount(2, "a@x.com")
		disabled.Status = StatusDisabled
		repo := newSIWCTestRepo(siwc, newer, older, pat, disabled)

		got, err := resolveOpenAISIWCManagementAccount(ctx, repo, &siwc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != 5 {
			t.Fatalf("want account 5, got %d", got.ID)
		}
	})

	t.Run("无邮箱或无候选报缺失", func(t *testing.T) {
		siwc := siwcTestAccount(1, "lonely@x.com")
		repo := newSIWCTestRepo(siwc, codexTestAccount(2, "someone@x.com"))
		if _, err := resolveOpenAISIWCManagementAccount(ctx, repo, &siwc); !errors.Is(err, errOpenAISIWCManagementAccountMissing) {
			t.Fatalf("want missing error, got %v", err)
		}

		noEmail := siwcTestAccount(3, "")
		if _, err := resolveOpenAISIWCManagementAccount(ctx, repo, &noEmail); !errors.Is(err, errOpenAISIWCManagementAccountMissing) {
			t.Fatalf("want missing error for no email, got %v", err)
		}
	})

	t.Run("非 siwc 账号拒绝", func(t *testing.T) {
		codex := codexTestAccount(1, "a@x.com")
		repo := newSIWCTestRepo(codex)
		if _, err := resolveOpenAISIWCManagementAccount(ctx, repo, &codex); err == nil {
			t.Fatal("expected error for non-siwc account")
		}
	})
}

func TestListOpenAISIWCSiblings(t *testing.T) {
	ctx := context.Background()
	a1 := siwcTestAccount(1, "a@x.com")
	a2 := siwcTestAccount(2, "A@x.com")
	b := siwcTestAccount(3, "b@x.com")
	codex := codexTestAccount(4, "a@x.com")
	repo := newSIWCTestRepo(a1, a2, b, codex)

	siblings, err := listOpenAISIWCSiblings(ctx, repo, "a@x.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(siblings) != 2 {
		t.Fatalf("want 2 siblings, got %d", len(siblings))
	}
	for _, s := range siblings {
		if s.ID != 1 && s.ID != 2 {
			t.Fatalf("unexpected sibling %d", s.ID)
		}
	}

	none, err := listOpenAISIWCSiblings(ctx, repo, "")
	if err != nil || none != nil {
		t.Fatalf("empty email should return nil, got %v %v", none, err)
	}
}

func TestExtraInt64Value(t *testing.T) {
	tests := []struct {
		in   any
		want int64
	}{
		{int64(7), 7}, {int(8), 8}, {float64(9), 9}, {"10", 10}, {" 11 ", 11}, {"x", 0}, {nil, 0}, {true, 0},
	}
	for _, tt := range tests {
		if got := extraInt64Value(tt.in); got != tt.want {
			t.Fatalf("extraInt64Value(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
