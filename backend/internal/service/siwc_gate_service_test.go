//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// siwcGateRepoStub 记录状态变更，并用内存账号列表支撑 ListByPlatform / GetByID。
type siwcGateRepoStub struct {
	siwcTestAccountRepo
	setErrorIDs   []int64
	clearErrorIDs []int64
	rateLimited   map[int64]time.Time
	extras        map[int64]map[string]any
}

func newSIWCGateRepoStub(accounts ...Account) *siwcGateRepoStub {
	r := &siwcGateRepoStub{rateLimited: map[int64]time.Time{}, extras: map[int64]map[string]any{}}
	r.accounts = accounts
	r.accountsByID = map[int64]*Account{}
	for i := range r.accounts {
		r.accountsByID[r.accounts[i].ID] = &r.accounts[i]
	}
	return r
}

func (r *siwcGateRepoStub) SetError(_ context.Context, id int64, _ string) error {
	r.setErrorIDs = append(r.setErrorIDs, id)
	if acc, ok := r.accountsByID[id]; ok {
		acc.Status = StatusError
		acc.Schedulable = false
	}
	return nil
}

func (r *siwcGateRepoStub) ClearError(_ context.Context, id int64) error {
	r.clearErrorIDs = append(r.clearErrorIDs, id)
	if acc, ok := r.accountsByID[id]; ok {
		acc.Status = StatusActive
	}
	return nil
}

func (r *siwcGateRepoStub) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.rateLimited[id] = resetAt
	return nil
}

func (r *siwcGateRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if r.extras[id] == nil {
		r.extras[id] = map[string]any{}
	}
	acc := r.accountsByID[id]
	if acc != nil && acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	for k, v := range updates {
		r.extras[id][k] = v
		if acc != nil {
			acc.Extra[k] = v
		}
	}
	return nil
}

// SetSchedulable 写回内存账号，供 Agent 切换 / 闸门恢复断言。SetError 同步置 false（与真实仓储一致）。
func (r *siwcGateRepoStub) SetSchedulable(_ context.Context, id int64, schedulable bool) error {
	if acc, ok := r.accountsByID[id]; ok {
		acc.Schedulable = schedulable
	}
	return nil
}

type siwcProbeStub struct {
	results []*SIWCProbeResult
	err     error
	calls   int
	bodies  [][]byte
}

func (p *siwcProbeStub) ProbeSIWC(_ context.Context, _ *Account, body []byte) (*SIWCProbeResult, error) {
	p.calls++
	p.bodies = append(p.bodies, body)
	if p.err != nil {
		return nil, p.err
	}
	if len(p.results) == 0 {
		return &SIWCProbeResult{Status: 200}, nil
	}
	res := p.results[0]
	if len(p.results) > 1 {
		p.results = p.results[1:]
	}
	return res, nil
}

type runtimeBlockerStub struct {
	blocked []int64
	cleared []int64
}

func (b *runtimeBlockerStub) BlockAccountScheduling(account *Account, _ time.Time, _ string) {
	b.blocked = append(b.blocked, account.ID)
}
func (b *runtimeBlockerStub) ClearAccountSchedulingBlock(id int64) { b.cleared = append(b.cleared, id) }

func newGateForTest(repo *siwcGateRepoStub, probe *siwcProbeStub) (*SIWCGateService, *runtimeBlockerStub) {
	gate := NewSIWCGateService(repo, probe, nil)
	blocker := &runtimeBlockerStub{}
	gate.SetAccountRuntimeBlocker(blocker)
	return gate, blocker
}

func TestSIWCGate_BlockAffectsAllSiblings(t *testing.T) {
	a1 := siwcTestAccount(1, "a@x.com")
	a2 := siwcTestAccount(2, "A@X.com")
	other := siwcTestAccount(3, "b@x.com")
	codex := codexTestAccount(4, "a@x.com")
	repo := newSIWCGateRepoStub(a1, a2, other, codex)
	gate, blocker := newGateForTest(repo, &siwcProbeStub{})

	fresh, err := gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "req-1")
	require.NoError(t, err)
	require.True(t, fresh)
	require.ElementsMatch(t, []int64{1, 2}, repo.setErrorIDs, "同邮箱全部 SIWC 行 SetError，管理凭据不动")
	require.ElementsMatch(t, []int64{1, 2}, blocker.blocked)
	state := ReadSIWCGateState(repo.accountsByID[2])
	require.NotNil(t, state)
	require.Equal(t, SIWCGateStateBlocked, state.State)
	require.Equal(t, "req-1", state.RequestID)
	require.NotNil(t, state.NextCheckAt)
	require.True(t, state.IsGuarded())
	require.Error(t, SIWCGateGuardError(repo.accountsByID[1]))
	require.NoError(t, SIWCGateGuardError(repo.accountsByID[4]))

	// 再次命中只更新 last_seen，不算 fresh
	fresh, err = gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "req-2")
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, "req-2", ReadSIWCGateState(repo.accountsByID[1]).RequestID)
}

func TestSIWCGate_RecheckStateMachine(t *testing.T) {
	a1 := siwcTestAccount(1, "a@x.com")
	repo := newSIWCGateRepoStub(a1)
	probe := &siwcProbeStub{}
	gate, _ := newGateForTest(repo, probe)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	gate.now = func() time.Time { return now }

	_, err := gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "r")
	require.NoError(t, err)

	// 未封禁的邮箱复检报错
	_, err = gate.Recheck(context.Background(), "nobody@x.com", true)
	require.Error(t, err)

	// 连续 4 次 403：前 3 次 6h，第 4 次起 24h
	probe.results = []*SIWCProbeResult{{Status: http.StatusForbidden, Code: SIWCGateCodeNotEligible, RequestID: "p1"}}
	for i := 1; i <= 4; i++ {
		state, err := gate.Recheck(context.Background(), "a@x.com", false)
		require.NoError(t, err)
		require.Equal(t, SIWCGateStateBlocked, state.State)
		require.Equal(t, i, state.Consecutive)
		expected := siwcGateRecheckInterval
		if i >= siwcGateRecheckSlowAfter {
			expected = siwcGateRecheckSlowPeriod
		}
		require.Equal(t, now.Add(expected), *state.NextCheckAt)
	}
	require.Len(t, ReadSIWCGateState(repo.accountsByID[1]).Checks, 4)

	// 其它错误不改结论
	probe.results = []*SIWCProbeResult{{Status: 500}}
	state, err := gate.Recheck(context.Background(), "a@x.com", false)
	require.NoError(t, err)
	require.Equal(t, SIWCGateStateBlocked, state.State)
	require.Equal(t, 4, state.Consecutive)

	// 探测报错同样不改结论
	probe.err = errors.New("network")
	state, err = gate.Recheck(context.Background(), "a@x.com", false)
	require.NoError(t, err)
	require.Equal(t, SIWCGateStateBlocked, state.State)
	probe.err = nil

	// 200 → restorable
	probe.results = []*SIWCProbeResult{{Status: 200}}
	state, err = gate.Recheck(context.Background(), "a@x.com", false)
	require.NoError(t, err)
	require.Equal(t, SIWCGateStateRestorable, state.State)
	require.Nil(t, state.NextCheckAt)
	require.True(t, state.IsGuarded(), "restorable 仍受守卫")
	require.Equal(t, StatusError, repo.accountsByID[1].Status, "复检通过不自动恢复")

	// restorable 再次 403 回到 blocked
	probe.results = []*SIWCProbeResult{{Status: http.StatusForbidden, Code: SIWCGateCodeNotEligible}}
	state, err = gate.Recheck(context.Background(), "a@x.com", false)
	require.NoError(t, err)
	require.Equal(t, SIWCGateStateBlocked, state.State)
}

func TestEvaluateSIWCFingerprint(t *testing.T) {
	at := time.Now()
	ok := evaluateSIWCFingerprint([]int64{18, 19, 18}, []int64{32, 31, 33}, at)
	require.Equal(t, SIWCFingerprintVerdictOK, ok.Verdict)
	require.Equal(t, 18.0, *ok.AstraP50)
	require.Equal(t, 32.0, *ok.LunaP50)

	suspect := evaluateSIWCFingerprint([]int64{30, 31, 31}, []int64{32, 31, 33}, at)
	require.Equal(t, SIWCFingerprintVerdictSuspect, suspect.Verdict)

	short := evaluateSIWCFingerprint([]int64{18, 19}, []int64{32, 31, 33}, at)
	require.Equal(t, SIWCFingerprintVerdictError, short.Verdict)
	require.Contains(t, short.Error, "insufficient")

	require.Nil(t, siwcMedian(nil))
	require.Equal(t, 2.5, *siwcMedian([]int64{4, 1, 2, 3}))
}

func TestSIWCGate_RestoreRequiresFingerprintOK(t *testing.T) {
	a1 := siwcTestAccount(1, "a@x.com")
	a2 := siwcTestAccount(2, "a@x.com")
	repo := newSIWCGateRepoStub(a1, a2)
	probe := &siwcProbeStub{}
	gate, blocker := newGateForTest(repo, probe)

	_, err := gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "r")
	require.NoError(t, err)

	// blocked 状态不能恢复
	_, err = gate.Restore(context.Background(), "a@x.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not passed")

	probe.results = []*SIWCProbeResult{{Status: 200}}
	_, err = gate.Recheck(context.Background(), "a@x.com", false)
	require.NoError(t, err)

	// 指纹 suspect：不恢复
	probe.results = []*SIWCProbeResult{
		{Status: 200, HasReasoning: true, ReasoningTokens: 30}, {Status: 200, HasReasoning: true, ReasoningTokens: 31}, {Status: 200, HasReasoning: true, ReasoningTokens: 31},
		{Status: 200, HasReasoning: true, ReasoningTokens: 32}, {Status: 200, HasReasoning: true, ReasoningTokens: 31}, {Status: 200, HasReasoning: true, ReasoningTokens: 33},
	}
	state, err := gate.Restore(context.Background(), "a@x.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "suspect")
	require.Equal(t, SIWCGateStateRestorable, state.State)
	require.Empty(t, repo.clearErrorIDs)
	require.Equal(t, SIWCFingerprintVerdictSuspect, ReadSIWCFingerprint(repo.accountsByID[1]).Verdict)

	// 指纹 ok：恢复全部兄弟行
	probe.results = []*SIWCProbeResult{
		{Status: 200, HasReasoning: true, ReasoningTokens: 18}, {Status: 200, HasReasoning: true, ReasoningTokens: 19}, {Status: 200, HasReasoning: true, ReasoningTokens: 18},
		{Status: 200, HasReasoning: true, ReasoningTokens: 32}, {Status: 200, HasReasoning: true, ReasoningTokens: 31}, {Status: 200, HasReasoning: true, ReasoningTokens: 33},
	}
	state, err = gate.Restore(context.Background(), "a@x.com")
	require.NoError(t, err)
	require.Equal(t, SIWCGateStateClear, state.State)
	require.ElementsMatch(t, []int64{1, 2}, repo.clearErrorIDs)
	require.ElementsMatch(t, []int64{1, 2}, blocker.cleared)
	require.False(t, state.IsGuarded())
	require.NoError(t, SIWCGateGuardError(repo.accountsByID[1]))
}

func TestSIWCGate_DueAndSummaryAndUsageLimit(t *testing.T) {
	a1 := siwcTestAccount(1, "a@x.com")
	b1 := siwcTestAccount(2, "b@x.com")
	repo := newSIWCGateRepoStub(a1, b1)
	probe := &siwcProbeStub{}
	gate, _ := newGateForTest(repo, probe)
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	gate.now = func() time.Time { return now }

	_, _ = gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "r")
	_, _ = gate.Block(context.Background(), repo.accountsByID[2], SIWCGateCodeNotEligible, "r")
	blocked, restorable, err := gate.Summary(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, blocked)
	require.Equal(t, 0, restorable)

	// 未到期不复检
	gate.Due(context.Background())
	require.Equal(t, 0, probe.calls)

	// 到期后两个邮箱各复检一次
	gate.now = func() time.Time { return now.Add(siwcGateRecheckInterval + time.Minute) }
	probe.results = []*SIWCProbeResult{{Status: 200}}
	gate.Due(context.Background())
	require.Equal(t, 2, probe.calls)
	blocked, restorable, _ = gate.Summary(context.Background())
	require.Equal(t, 0, blocked)
	require.Equal(t, 2, restorable)

	// 429 usage limit：兄弟行一起限流，不进闸门
	a2 := siwcTestAccount(3, "a@x.com")
	repo2 := newSIWCGateRepoStub(siwcTestAccount(1, "a@x.com"), a2)
	gate2, _ := newGateForTest(repo2, &siwcProbeStub{})
	resetAt := now.Add(5 * time.Hour)
	gate2.HandleUsageLimit(context.Background(), repo2.accountsByID[1], resetAt)
	require.Equal(t, resetAt, repo2.rateLimited[1])
	require.Equal(t, resetAt, repo2.rateLimited[3])
	require.Nil(t, ReadSIWCGateState(repo2.accountsByID[1]))
}

func TestClassifyOpenAISIWCUpstreamErrorAndReset(t *testing.T) {
	require.Equal(t, openAISIWCErrorNotEligible, classifyOpenAISIWCUpstreamError(403, []byte(`{"error":{"code":"subscription_sharing_user_not_eligible","message":"x"}}`)))
	require.Equal(t, openAISIWCErrorUsageLimit, classifyOpenAISIWCUpstreamError(429, []byte(`{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`)))
	require.Equal(t, openAISIWCErrorUnsupportedCapability, classifyOpenAISIWCUpstreamError(400, []byte(`{"error":{"code":"subscription_sharing_unsupported_capability"}}`)))
	// message 里出现同样文字但 code 不符：不触发
	require.Equal(t, openAISIWCErrorNone, classifyOpenAISIWCUpstreamError(403, []byte(`{"error":{"code":"other","message":"subscription_sharing_user_not_eligible"}}`)))
	require.Equal(t, openAISIWCErrorNone, classifyOpenAISIWCUpstreamError(200, nil))

	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("x-codex-primary-reset-after-seconds", "120")
	require.Equal(t, now.Add(2*time.Minute), resolveOpenAISIWCUsageLimitReset(now, h, nil))
	require.Equal(t, now.Add(90*time.Second), resolveOpenAISIWCUsageLimitReset(now, nil, []byte(`{"error":{"resets_in_seconds":90}}`)))
	require.Equal(t, now.Add(siwcGateUsageLimitWeekly), resolveOpenAISIWCUsageLimitReset(now, nil, []byte(`{"error":{"message":"weekly limit reached"}}`)))
	require.Equal(t, now.Add(siwcGateUsageLimitDefault), resolveOpenAISIWCUsageLimitReset(now, nil, []byte(`{"error":{"code":"x"}}`)))
}

func TestOpenAIGatewayService_HandleOpenAISIWCUpstreamError(t *testing.T) {
	a1 := siwcTestAccount(1, "a@x.com")
	repo := newSIWCGateRepoStub(a1)
	gate, _ := newGateForTest(repo, &siwcProbeStub{})
	svc := &OpenAIGatewayService{}
	svc.SetSIWCGate(gate)

	handled, disable := svc.handleOpenAISIWCUpstreamError(context.Background(), repo.accountsByID[1], 403, http.Header{"X-Request-Id": {"req-9"}}, []byte(`{"error":{"code":"subscription_sharing_user_not_eligible"}}`))
	require.True(t, handled)
	require.True(t, disable)
	require.Equal(t, []int64{1}, repo.setErrorIDs)
	require.Equal(t, "req-9", ReadSIWCGateState(repo.accountsByID[1]).RequestID)

	handled, disable = svc.handleOpenAISIWCUpstreamError(context.Background(), repo.accountsByID[1], 429, nil, []byte(`{"error":{"code":"subscription_sharing_usage_limit_exceeded","resets_in_seconds":60}}`))
	require.True(t, handled)
	require.True(t, disable)
	require.NotZero(t, repo.rateLimited[1])

	handled, disable = svc.handleOpenAISIWCUpstreamError(context.Background(), repo.accountsByID[1], 400, nil, []byte(`{"error":{"code":"subscription_sharing_unsupported_capability"}}`))
	require.True(t, handled)
	require.False(t, disable)

	handled, _ = svc.handleOpenAISIWCUpstreamError(context.Background(), repo.accountsByID[1], 500, nil, []byte(`{}`))
	require.False(t, handled)

	codex := codexTestAccount(2, "a@x.com")
	handled, _ = svc.handleOpenAISIWCUpstreamError(context.Background(), &codex, 403, nil, []byte(`{"error":{"code":"subscription_sharing_user_not_eligible"}}`))
	require.False(t, handled)
}

func TestParseSIWCReasoningTokens(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens_details\":{\"reasoning_tokens\":18}}}}\n\ndata: [DONE]\n"
	tokens, ok := parseSIWCReasoningTokens([]byte(sse))
	require.True(t, ok)
	require.Equal(t, int64(18), tokens)

	tokens, ok = parseSIWCReasoningTokens([]byte(`{"usage":{"output_tokens_details":{"reasoning_tokens":7}}}`))
	require.True(t, ok)
	require.Equal(t, int64(7), tokens)

	_, ok = parseSIWCReasoningTokens([]byte("data: {\"type\":\"response.created\"}\n"))
	require.False(t, ok)
	_, ok = parseSIWCReasoningTokens(nil)
	require.False(t, ok)
}

func TestRateLimitService_RecoverAccountStateRespectsSIWCGate(t *testing.T) {
	a1 := siwcTestAccount(1, "a@x.com")
	a1.Status = StatusError
	a1.Extra = map[string]any{"siwc_gate": map[string]any{"state": "restorable"}}
	repo := newSIWCGateRepoStub(a1)
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	_, err := svc.RecoverAccountState(context.Background(), 1, AccountRecoveryOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "eligibility gate")
	require.Empty(t, repo.clearErrorIDs)
}

func TestSIWCGate_RestoreReenablesOnlyActiveAgent(t *testing.T) {
	active := siwcTestAccount(1, "a@x.com")
	active.Schedulable = true
	standby := siwcTestAccount(2, "a@x.com")
	standby.Extra = map[string]any{siwcAgentRoleExtraKey: SIWCAgentRoleStandby}
	repo := newSIWCGateRepoStub(active, standby)
	probe := &siwcProbeStub{}
	gate, _ := newGateForTest(repo, probe)

	_, err := gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "r")
	require.NoError(t, err)
	require.False(t, repo.accountsByID[1].Schedulable)

	probe.results = []*SIWCProbeResult{
		{Status: 200},
		{Status: 200, HasReasoning: true, ReasoningTokens: 18}, {Status: 200, HasReasoning: true, ReasoningTokens: 19}, {Status: 200, HasReasoning: true, ReasoningTokens: 18},
		{Status: 200, HasReasoning: true, ReasoningTokens: 32}, {Status: 200, HasReasoning: true, ReasoningTokens: 31}, {Status: 200, HasReasoning: true, ReasoningTokens: 33},
	}
	_, err = gate.Recheck(context.Background(), "a@x.com", false)
	require.NoError(t, err)
	_, err = gate.Restore(context.Background(), "a@x.com")
	require.NoError(t, err)
	require.True(t, repo.accountsByID[1].Schedulable, "活跃 Agent 恢复后必须重新可调度（ClearError 不会恢复 schedulable）")
	require.False(t, repo.accountsByID[2].Schedulable, "待命 Agent 恢复后仍不可调度")
}

func TestSIWCGate_AutoRestoreAfterRecheck(t *testing.T) {
	okSamples := []*SIWCProbeResult{
		{Status: 200}, // 复检
		{Status: 200, HasReasoning: true, ReasoningTokens: 18}, {Status: 200, HasReasoning: true, ReasoningTokens: 19}, {Status: 200, HasReasoning: true, ReasoningTokens: 18},
		{Status: 200, HasReasoning: true, ReasoningTokens: 32}, {Status: 200, HasReasoning: true, ReasoningTokens: 31}, {Status: 200, HasReasoning: true, ReasoningTokens: 33},
	}

	t.Run("开关打开且指纹 ok：自动恢复", func(t *testing.T) {
		repo := newSIWCGateRepoStub(siwcTestAccount(1, "a@x.com"), siwcTestAccount(2, "a@x.com"))
		probe := &siwcProbeStub{}
		gate, blocker := newGateForTest(repo, probe)
		gate.SetAutoRestore(true)
		_, _ = gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "r")

		probe.results = append([]*SIWCProbeResult(nil), okSamples...)
		state, err := gate.Recheck(context.Background(), "a@x.com", false)
		require.NoError(t, err)
		require.Equal(t, SIWCGateStateClear, state.State)
		require.ElementsMatch(t, []int64{1, 2}, repo.clearErrorIDs)
		require.ElementsMatch(t, []int64{1, 2}, blocker.cleared)
		require.Equal(t, 7, probe.calls, "1 次复检 + 6 次指纹")
	})

	t.Run("开关打开但指纹 suspect：保持待恢复", func(t *testing.T) {
		repo := newSIWCGateRepoStub(siwcTestAccount(1, "a@x.com"))
		probe := &siwcProbeStub{}
		gate, _ := newGateForTest(repo, probe)
		gate.SetAutoRestore(true)
		_, _ = gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "r")

		probe.results = []*SIWCProbeResult{
			{Status: 200},
			{Status: 200, HasReasoning: true, ReasoningTokens: 31}, {Status: 200, HasReasoning: true, ReasoningTokens: 30}, {Status: 200, HasReasoning: true, ReasoningTokens: 31},
			{Status: 200, HasReasoning: true, ReasoningTokens: 32}, {Status: 200, HasReasoning: true, ReasoningTokens: 31}, {Status: 200, HasReasoning: true, ReasoningTokens: 33},
		}
		state, err := gate.Recheck(context.Background(), "a@x.com", false)
		require.NoError(t, err)
		require.Equal(t, SIWCGateStateRestorable, state.State)
		require.Equal(t, SIWCFingerprintVerdictSuspect, state.Fingerprint.Verdict)
		require.Empty(t, repo.clearErrorIDs)
		require.Equal(t, StatusError, repo.accountsByID[1].Status)
	})

	t.Run("开关关闭：复检通过只到待恢复，不跑指纹", func(t *testing.T) {
		repo := newSIWCGateRepoStub(siwcTestAccount(1, "a@x.com"))
		probe := &siwcProbeStub{}
		gate, _ := newGateForTest(repo, probe)
		_, _ = gate.Block(context.Background(), repo.accountsByID[1], SIWCGateCodeNotEligible, "r")

		probe.results = []*SIWCProbeResult{{Status: 200}}
		state, err := gate.Recheck(context.Background(), "a@x.com", false)
		require.NoError(t, err)
		require.Equal(t, SIWCGateStateRestorable, state.State)
		require.Equal(t, 1, probe.calls)
		require.Empty(t, repo.clearErrorIDs)
	})
}
