package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

// SIWC 资格闸门与降智指纹（移植自 cpa 的 eligibility_gate.py，6 天线上经验）。
//
// 403 subscription_sharing_user_not_eligible 在上游网关准入阶段返回、按 ChatGPT 账号生效，
// 同邮箱从没用过的备用凭据也一起 403；上游不冷却账号，不处理会被调度器一直轮到。
// 实测 18～24 小时自行解除，重新授权无效。
//
// 策略：命中即按邮箱 SetError 全部 SIWC 行（status=error，只有 ClearError 能解）；
// 每 6 小时用固定极小请求复检，连续 4 次仍 403 降为 24 小时；复检通过进入 restorable，
// 恢复调度前必须跑 astra/luna 推理 token 指纹，astra 中位数低于 luna 的 75% 才放行。
const (
	siwcGateExtraKey        = "siwc_gate"
	siwcFingerprintExtraKey = "siwc_fingerprint"

	SIWCGateStateBlocked    = "blocked"
	SIWCGateStateRestorable = "restorable"
	SIWCGateStateClear      = "clear"

	SIWCGateCodeNotEligible = "subscription_sharing_user_not_eligible"
	SIWCGateCodeUsageLimit  = "subscription_sharing_usage_limit_exceeded"

	SIWCFingerprintVerdictOK      = "ok"
	SIWCFingerprintVerdictSuspect = "suspect"
	SIWCFingerprintVerdictError   = "error"

	siwcGateRecheckInterval   = 6 * time.Hour
	siwcGateRecheckSlowAfter  = 4
	siwcGateRecheckSlowPeriod = 24 * time.Hour
	siwcGateManualCooldown    = 60 * time.Second
	siwcGateCheckHistory      = 20
	siwcFingerprintSamples    = 3
	siwcFingerprintRatio      = 0.75
	siwcFingerprintLockTTL    = 10 * time.Minute
	siwcGateLoopInterval      = time.Minute
	siwcGateLeaderLockKey     = "siwc_gate:recheck"
	siwcGateLeaderLockTTL     = 5 * time.Minute
	siwcGateProbeTimeout      = 60 * time.Second
	siwcGateErrorPrefix       = "siwc_not_eligible"
	siwcGateUsageLimitDefault = 5 * time.Hour
	siwcGateUsageLimitWeekly  = 7 * 24 * time.Hour
)

// siwcFingerprintPrompt 与 cpa eligibility_gate.py 逐字一致（糖果题，答案 12）。
const siwcFingerprintPrompt = "盒子里有 24 颗糖。小明取走总数的四分之一，小红随后取走剩余糖的一半，最后小明放回 3 颗。盒子里现在有多少颗糖？只输出最终数字，不要任何解释。"

const (
	siwcFingerprintModelAstra = "gpt-6-astra"
	siwcFingerprintModelLuna  = "gpt-6-luna"
)

// SIWCGateCheck 一次复检记录。
type SIWCGateCheck struct {
	At        time.Time `json:"at"`
	Status    int       `json:"status"`
	Code      string    `json:"code,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
}

// SIWCFingerprint astra/luna 推理 token 指纹结果。
type SIWCFingerprint struct {
	At       time.Time `json:"at"`
	Astra    []int64   `json:"astra"`
	Luna     []int64   `json:"luna"`
	AstraP50 *float64  `json:"astra_p50"`
	LunaP50  *float64  `json:"luna_p50"`
	Verdict  string    `json:"verdict"`
	Error    string    `json:"error,omitempty"`
}

// SIWCGateState 闸门状态，真源在 accounts.extra.siwc_gate。
type SIWCGateState struct {
	State                 string           `json:"state"`
	Code                  string           `json:"code,omitempty"`
	RequestID             string           `json:"request_id,omitempty"`
	BlockedAt             *time.Time       `json:"blocked_at,omitempty"`
	LastSeenAt            *time.Time       `json:"last_seen_at,omitempty"`
	LastCheckAt           *time.Time       `json:"last_check_at,omitempty"`
	NextCheckAt           *time.Time       `json:"next_check_at,omitempty"`
	Consecutive           int              `json:"consecutive"`
	Checks                []SIWCGateCheck  `json:"checks,omitempty"`
	Fingerprint           *SIWCFingerprint `json:"fingerprint,omitempty"`
	ClearedAt             *time.Time       `json:"cleared_at,omitempty"`
	ManualCooldownRemains int64            `json:"manual_cooldown_remaining,omitempty"`
}

// IsGuarded 报告闸门是否仍在生效（blocked / restorable 都不允许绕过指纹恢复）。
func (s *SIWCGateState) IsGuarded() bool {
	return s != nil && (s.State == SIWCGateStateBlocked || s.State == SIWCGateStateRestorable)
}

// SIWCProbeResult 一次探测的结果。
type SIWCProbeResult struct {
	Status          int
	Code            string
	RequestID       string
	ReasoningTokens int64
	HasReasoning    bool
}

// SIWCProbe 由 OpenAIGatewayService 实现：用账号的 SIWC 凭据向 api.openai.com 发固定请求。
type SIWCProbe interface {
	ProbeSIWC(ctx context.Context, account *Account, body []byte) (*SIWCProbeResult, error)
}

// SIWCGateService 资格闸门服务。
type SIWCGateService struct {
	accountRepo    AccountRepository
	probe          SIWCProbe
	audit          *AuditLogService
	lockCache      LeaderLockCache
	db             *sql.DB
	runtimeBlocker AccountRuntimeBlocker
	instanceID     string
	now            func() time.Time
	// autoRestore 复检通过后自动跑指纹，ok 即恢复调度（gateway.siwc_auto_restore）。
	autoRestore bool

	loopInterval time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
}

// NewSIWCGateService 构造闸门服务。
func NewSIWCGateService(accountRepo AccountRepository, probe SIWCProbe, audit *AuditLogService) *SIWCGateService {
	return &SIWCGateService{
		accountRepo:  accountRepo,
		probe:        probe,
		audit:        audit,
		instanceID:   uuid.NewString(),
		now:          time.Now,
		loopInterval: siwcGateLoopInterval,
		stopCh:       make(chan struct{}),
	}
}

// SetLeaderLock 注入跨副本 leader 锁（复检 / 指纹互斥 / 手动冷却都用它）。
func (s *SIWCGateService) SetLeaderLock(cache LeaderLockCache, db *sql.DB) {
	s.lockCache = cache
	s.db = db
}

// SetAutoRestore 设置复检通过后是否自动恢复调度。
func (s *SIWCGateService) SetAutoRestore(enabled bool) {
	s.autoRestore = enabled
}

// SetAccountRuntimeBlocker 注入运行时调度阻断器。
func (s *SIWCGateService) SetAccountRuntimeBlocker(blocker AccountRuntimeBlocker) {
	s.runtimeBlocker = blocker
}

// Start 启动后台复检循环。
func (s *SIWCGateService) Start() {
	if s == nil || s.accountRepo == nil || s.loopInterval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.loopInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.Due(context.Background())
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止后台循环。
func (s *SIWCGateService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// ---------- 状态读写 ----------

// ReadSIWCGateState 从账号 extra 解析闸门状态；不存在返回 nil。
func ReadSIWCGateState(account *Account) *SIWCGateState {
	if account == nil || account.Extra == nil {
		return nil
	}
	return decodeSIWCGateState(account.Extra[siwcGateExtraKey])
}

// ReadSIWCFingerprint 从账号 extra 解析独立指纹结果。
func ReadSIWCFingerprint(account *Account) *SIWCFingerprint {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[siwcFingerprintExtraKey]
	if !ok || raw == nil {
		return nil
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var fp SIWCFingerprint
	if err := json.Unmarshal(payload, &fp); err != nil {
		return nil
	}
	return &fp
}

func decodeSIWCGateState(raw any) *SIWCGateState {
	if raw == nil {
		return nil
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var state SIWCGateState
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil
	}
	if strings.TrimSpace(state.State) == "" {
		return nil
	}
	return &state
}

// toExtraValue 把结构体转成 JSONB 可存的 map。
func siwcToExtraValue(v any) any {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil
	}
	return out
}

func (s *SIWCGateService) persistGate(ctx context.Context, accountID int64, state *SIWCGateState) error {
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{siwcGateExtraKey: siwcToExtraValue(state)})
}

// ---------- 守卫 ----------

// SIWCGateGuardError 当账号处于闸门状态时返回 409，供 RecoverAccountState / ClearError / Test 拦截。
func SIWCGateGuardError(account *Account) error {
	if account == nil || !account.IsOpenAISIWC() {
		return nil
	}
	state := ReadSIWCGateState(account)
	if !state.IsGuarded() {
		return nil
	}
	return infraerrors.New(http.StatusConflict, "SIWC_GATE_BLOCKED",
		"account is held by the SIWC eligibility gate; run recheck and fingerprint, then restore from the gate panel")
}

// ---------- Block ----------

// Block 命中 403 user_not_eligible：按邮箱封禁全部 SIWC 行。返回是否为首次封禁。
func (s *SIWCGateService) Block(ctx context.Context, account *Account, code, requestID string) (bool, error) {
	if s == nil || s.accountRepo == nil || account == nil || !account.IsOpenAISIWC() {
		return false, nil
	}
	email := openAIAccountCredentialEmail(account)
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, email)
	if err != nil {
		return false, err
	}
	if len(siblings) == 0 {
		siblings = []Account{*account}
	}
	now := s.now()
	fresh := false
	blockedIDs := make([]int64, 0, len(siblings))
	for i := range siblings {
		sib := &siblings[i]
		state := ReadSIWCGateState(sib)
		if state == nil || state.State != SIWCGateStateBlocked {
			fresh = true
			next := now.Add(siwcGateRecheckInterval)
			state = &SIWCGateState{
				State:       SIWCGateStateBlocked,
				Code:        code,
				RequestID:   requestID,
				BlockedAt:   &now,
				LastSeenAt:  &now,
				NextCheckAt: &next,
			}
		} else {
			state.LastSeenAt = &now
			if requestID != "" {
				state.RequestID = requestID
			}
		}
		if err := s.accountRepo.SetError(ctx, sib.ID, fmt.Sprintf("%s: %s req=%s", siwcGateErrorPrefix, code, requestID)); err != nil {
			slog.Warn("siwc_gate.set_error_failed", "account_id", sib.ID, "error", err)
			continue
		}
		if err := s.persistGate(ctx, sib.ID, state); err != nil {
			slog.Warn("siwc_gate.persist_failed", "account_id", sib.ID, "error", err)
		}
		if s.runtimeBlocker != nil {
			s.runtimeBlocker.BlockAccountScheduling(sib, time.Time{}, siwcGateErrorPrefix)
		}
		blockedIDs = append(blockedIDs, sib.ID)
	}
	s.recordAudit("system.siwc_gate.block", email, http.StatusForbidden, map[string]any{
		"email": email, "account_ids": blockedIDs, "code": code, "request_id": requestID, "fresh": fresh,
	})
	slog.Warn("siwc_gate.blocked", "email", email, "account_ids", blockedIDs, "code", code, "request_id", requestID, "fresh", fresh)
	return fresh, nil
}

// ---------- Recheck ----------

func (s *SIWCGateService) tryCooldown(ctx context.Context, key string, ttl time.Duration) bool {
	if s.lockCache == nil {
		return true
	}
	ok, err := s.lockCache.TryAcquireLeaderLock(ctx, key, s.instanceID, ttl)
	if err != nil {
		// 冷却只是节流，Redis 故障不阻断操作
		return true
	}
	return ok
}

// siwcProbeBody 构造固定探测体（与 cpa 的 PROBE_BODIES 一致）。
func siwcProbeBody(model, text string, maxOutputTokens int) []byte {
	body := map[string]any{
		"model":        model,
		"instructions": "You are a helpful assistant.",
		"stream":       true,
		"store":        false,
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		}},
	}
	if maxOutputTokens > 0 {
		body["max_output_tokens"] = maxOutputTokens
	}
	payload, _ := json.Marshal(body)
	return payload
}

func (s *SIWCGateService) blockedSiblings(ctx context.Context, email string) ([]Account, *SIWCGateState, error) {
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, email)
	if err != nil {
		return nil, nil, err
	}
	var state *SIWCGateState
	for i := range siblings {
		if st := ReadSIWCGateState(&siblings[i]); st.IsGuarded() {
			if state == nil || st.State == SIWCGateStateBlocked {
				state = st
			}
		}
	}
	return siblings, state, nil
}

// Recheck 对同邮箱账号做一次资格复检（只探 1 份凭据），结果同步到全部兄弟行。
func (s *SIWCGateService) Recheck(ctx context.Context, email string, manual bool) (*SIWCGateState, error) {
	if s == nil || s.accountRepo == nil || s.probe == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "SIWC_GATE_NOT_CONFIGURED", "siwc gate service is not configured")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	siblings, state, err := s.blockedSiblings(ctx, email)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_GATE_NOT_BLOCKED", "account is not held by the siwc eligibility gate")
	}
	if manual && !s.tryCooldown(ctx, "siwc_gate:manual:"+email, siwcGateManualCooldown) {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_GATE_RECHECK_COOLDOWN", "manual recheck is rate limited; try again in a minute")
	}
	probeAccount := &siblings[0]
	for i := range siblings {
		if strings.TrimSpace(siblings[i].GetCredential("refresh_token")) != "" {
			probeAccount = &siblings[i]
			break
		}
	}

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siwcGateProbeTimeout)
	defer cancel()
	result, probeErr := s.probe.ProbeSIWC(probeCtx, probeAccount, siwcProbeBody(siwcFingerprintModelLuna, "1", 16))
	now := s.now()
	check := SIWCGateCheck{At: now}
	if probeErr != nil {
		check.Status = 0
		check.Code = "probe_error"
	} else {
		check.Status = result.Status
		check.Code = result.Code
		check.RequestID = result.RequestID
	}

	state.LastCheckAt = &now
	state.Checks = append(state.Checks, check)
	if len(state.Checks) > siwcGateCheckHistory {
		state.Checks = state.Checks[len(state.Checks)-siwcGateCheckHistory:]
	}
	switch {
	case probeErr == nil && result.Status == http.StatusForbidden && result.Code == SIWCGateCodeNotEligible:
		// 仍被封：计数并安排下一次
		state.State = SIWCGateStateBlocked
		state.Consecutive++
		interval := siwcGateRecheckInterval
		if state.Consecutive >= siwcGateRecheckSlowAfter {
			interval = siwcGateRecheckSlowPeriod
		}
		next := now.Add(interval)
		state.NextCheckAt = &next
	case probeErr == nil && result.Status >= 200 && result.Status < 300:
		// 解封：进入待恢复，等指纹通过后人工 / 自动恢复
		state.State = SIWCGateStateRestorable
		state.Consecutive = 0
		state.NextCheckAt = nil
	default:
		// 其它错误（网络、5xx、其它 4xx）不改结论，稍后再查
		next := now.Add(siwcGateRecheckInterval)
		state.NextCheckAt = &next
	}
	for i := range siblings {
		if err := s.persistGate(ctx, siblings[i].ID, state); err != nil {
			slog.Warn("siwc_gate.persist_failed", "account_id", siblings[i].ID, "error", err)
		}
	}
	s.recordAudit("system.siwc_gate.recheck", email, check.Status, map[string]any{
		"email": email, "status": check.Status, "code": check.Code, "request_id": check.RequestID, "state": state.State, "manual": manual,
	})
	// 自动恢复：复检通过后立即跑指纹，ok 才恢复；suspect / error 保持 restorable 等人工处理。
	if s.autoRestore && state.State == SIWCGateStateRestorable {
		restored, restoreErr := s.Restore(ctx, email)
		if restoreErr != nil {
			slog.Warn("siwc_gate.auto_restore_skipped", "email", email, "error", restoreErr)
		} else {
			slog.Info("siwc_gate.auto_restored", "email", email)
		}
		if restored != nil {
			return restored, nil
		}
	}
	return state, nil
}

// ---------- Fingerprint ----------

func siwcMedian(values []int64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var median float64
	if n := len(sorted); n%2 == 1 {
		median = float64(sorted[n/2])
	} else {
		median = float64(sorted[n/2-1]+sorted[n/2]) / 2
	}
	return &median
}

// evaluateSIWCFingerprint 按 cpa 规则判定：样本不足 → error；astra_p50 <= 0.75*luna_p50 → ok；否则 suspect。
func evaluateSIWCFingerprint(astra, luna []int64, at time.Time) *SIWCFingerprint {
	fp := &SIWCFingerprint{At: at, Astra: astra, Luna: luna, AstraP50: siwcMedian(astra), LunaP50: siwcMedian(luna)}
	switch {
	case len(astra) < siwcFingerprintSamples || len(luna) < siwcFingerprintSamples:
		fp.Verdict = SIWCFingerprintVerdictError
		fp.Error = fmt.Sprintf("insufficient samples: astra=%d luna=%d", len(astra), len(luna))
	case fp.LunaP50 != nil && *fp.LunaP50 > 0 && fp.AstraP50 != nil && *fp.AstraP50 <= siwcFingerprintRatio**fp.LunaP50:
		fp.Verdict = SIWCFingerprintVerdictOK
	default:
		fp.Verdict = SIWCFingerprintVerdictSuspect
	}
	return fp
}

// Fingerprint 对任意 SIWC 账号跑 astra×3 + luna×3，比较推理 token 中位数，结果写 extra.siwc_fingerprint。
func (s *SIWCGateService) Fingerprint(ctx context.Context, account *Account) (*SIWCFingerprint, error) {
	if s == nil || s.accountRepo == nil || s.probe == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "SIWC_GATE_NOT_CONFIGURED", "siwc gate service is not configured")
	}
	if account == nil || !account.IsOpenAISIWC() {
		return nil, infraerrors.New(http.StatusBadRequest, "SIWC_FINGERPRINT_NOT_SIWC", "account is not a siwc account")
	}
	email := openAIAccountCredentialEmail(account)
	lockKey := "siwc_gate:fp:" + email
	if s.lockCache != nil {
		ok, err := s.lockCache.TryAcquireLeaderLock(ctx, lockKey, s.instanceID, siwcFingerprintLockTTL)
		if err == nil && !ok {
			return nil, infraerrors.New(http.StatusConflict, "SIWC_FINGERPRINT_RUNNING", "a fingerprint run is already in progress for this account")
		}
		if err == nil {
			defer func() {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = s.lockCache.ReleaseLeaderLock(releaseCtx, lockKey, s.instanceID)
			}()
		}
	}

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*siwcGateProbeTimeout)
	defer cancel()
	astra := s.sampleReasoningTokens(probeCtx, account, siwcFingerprintModelAstra)
	luna := s.sampleReasoningTokens(probeCtx, account, siwcFingerprintModelLuna)
	fp := evaluateSIWCFingerprint(astra, luna, s.now())

	updates := map[string]any{siwcFingerprintExtraKey: siwcToExtraValue(fp)}
	if state := ReadSIWCGateState(account); state.IsGuarded() {
		state.Fingerprint = fp
		updates[siwcGateExtraKey] = siwcToExtraValue(state)
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("siwc_gate.fingerprint_persist_failed", "account_id", account.ID, "error", err)
	}
	s.recordAudit("system.siwc_gate.fingerprint", email, http.StatusOK, map[string]any{
		"email": email, "account_id": account.ID, "verdict": fp.Verdict, "astra": astra, "luna": luna,
	})
	return fp, nil
}

func (s *SIWCGateService) sampleReasoningTokens(ctx context.Context, account *Account, model string) []int64 {
	samples := make([]int64, 0, siwcFingerprintSamples)
	body := siwcProbeBody(model, siwcFingerprintPrompt, 0)
	for i := 0; i < siwcFingerprintSamples; i++ {
		if ctx.Err() != nil {
			break
		}
		result, err := s.probe.ProbeSIWC(ctx, account, body)
		if err != nil {
			slog.Warn("siwc_gate.fingerprint_probe_failed", "account_id", account.ID, "model", model, "error", err)
			continue
		}
		if result.Status < 200 || result.Status >= 300 || !result.HasReasoning {
			slog.Warn("siwc_gate.fingerprint_sample_invalid", "account_id", account.ID, "model", model, "status", result.Status, "code", result.Code)
			continue
		}
		samples = append(samples, result.ReasoningTokens)
	}
	return samples
}

// ---------- Restore ----------

// Restore 仅 restorable 可恢复：先跑指纹，ok 才 ClearError 全部兄弟行并清闸门。
func (s *SIWCGateService) Restore(ctx context.Context, email string) (*SIWCGateState, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "SIWC_GATE_NOT_CONFIGURED", "siwc gate service is not configured")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	siblings, state, err := s.blockedSiblings(ctx, email)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_GATE_NOT_BLOCKED", "account is not held by the siwc eligibility gate")
	}
	if state.State != SIWCGateStateRestorable {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_GATE_NOT_RESTORABLE", "account has not passed the eligibility recheck yet")
	}
	fp, err := s.Fingerprint(ctx, &siblings[0])
	if err != nil {
		return nil, err
	}
	state.Fingerprint = fp
	if fp.Verdict != SIWCFingerprintVerdictOK {
		for i := range siblings {
			_ = s.persistGate(ctx, siblings[i].ID, state)
		}
		code := "SIWC_GATE_FINGERPRINT_SUSPECT"
		if fp.Verdict == SIWCFingerprintVerdictError {
			code = "SIWC_GATE_FINGERPRINT_ERROR"
		}
		return state, infraerrors.Newf(http.StatusConflict, code, "fingerprint verdict %s; account stays paused", fp.Verdict)
	}

	now := s.now()
	state.State = SIWCGateStateClear
	state.ClearedAt = &now
	state.NextCheckAt = nil
	restored := make([]int64, 0, len(siblings))
	// 账号被手动暂停：闸门恢复只清错误，不打开调度
	accountPaused := siwcGroupPaused(siblings)
	for i := range siblings {
		sib := &siblings[i]
		if err := s.accountRepo.ClearError(ctx, sib.ID); err != nil {
			slog.Warn("siwc_gate.clear_error_failed", "account_id", sib.ID, "error", err)
			continue
		}
		if err := s.persistGate(ctx, sib.ID, state); err != nil {
			slog.Warn("siwc_gate.persist_failed", "account_id", sib.ID, "error", err)
		}
		// Block 时 SetError 已把 schedulable 置 false，ClearError 不会恢复它；
		// 只恢复活跃 Agent，待命 Agent 保持不可调度（同一时刻只有一个 Agent 接流量）。
		if !sib.IsOpenAISIWCStandby() && !accountPaused {
			if err := s.accountRepo.SetSchedulable(ctx, sib.ID, true); err != nil {
				slog.Warn("siwc_gate.restore_schedulable_failed", "account_id", sib.ID, "error", err)
			}
		}
		if s.runtimeBlocker != nil {
			s.runtimeBlocker.ClearAccountSchedulingBlock(sib.ID)
		}
		restored = append(restored, sib.ID)
	}
	s.recordAudit("system.siwc_gate.restore", email, http.StatusOK, map[string]any{
		"email": email, "account_ids": restored, "verdict": fp.Verdict,
	})
	return state, nil
}

// ---------- 后台复检 ----------

// Due 扫描到期的 blocked 账号并复检；多副本下由 leader 锁单飞。
func (s *SIWCGateService) Due(ctx context.Context) {
	if s == nil || s.accountRepo == nil || s.probe == nil {
		return
	}
	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, siwcGateLeaderLockKey, s.instanceID, siwcGateLeaderLockTTL)
	if !ok {
		return
	}
	defer release()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		slog.Warn("siwc_gate.list_accounts_failed", "error", err)
		return
	}
	now := s.now()
	dueEmails := map[string]bool{}
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsOpenAISIWC() {
			continue
		}
		state := ReadSIWCGateState(acc)
		if state == nil || state.State != SIWCGateStateBlocked || state.NextCheckAt == nil || state.NextCheckAt.After(now) {
			continue
		}
		dueEmails[openAIAccountCredentialEmail(acc)] = true
	}
	for email := range dueEmails {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.Recheck(ctx, email, false); err != nil {
			slog.Warn("siwc_gate.recheck_failed", "email", email, "error", err)
		}
	}
}

// Summary 返回被封禁 / 待恢复的账号数（按账号行计）。
func (s *SIWCGateService) Summary(ctx context.Context) (blocked, restorable int, err error) {
	if s == nil || s.accountRepo == nil {
		return 0, 0, nil
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return 0, 0, err
	}
	for i := range accounts {
		if !accounts[i].IsOpenAISIWC() {
			continue
		}
		switch st := ReadSIWCGateState(&accounts[i]); {
		case st == nil:
		case st.State == SIWCGateStateBlocked:
			blocked++
		case st.State == SIWCGateStateRestorable:
			restorable++
		}
	}
	return blocked, restorable, nil
}

// HandleUsageLimit 429 subscription_sharing_usage_limit_exceeded：按窗口冷却全部兄弟行，不进闸门。
func (s *SIWCGateService) HandleUsageLimit(ctx context.Context, account *Account, resetAt time.Time) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, openAIAccountCredentialEmail(account))
	if err != nil || len(siblings) == 0 {
		siblings = []Account{*account}
	}
	for i := range siblings {
		if err := s.accountRepo.SetRateLimited(ctx, siblings[i].ID, resetAt); err != nil {
			slog.Warn("siwc_gate.usage_limit_set_failed", "account_id", siblings[i].ID, "error", err)
		}
	}
}

func (s *SIWCGateService) recordAudit(action, email string, status int, extra map[string]any) {
	if s == nil || s.audit == nil {
		return
	}
	s.audit.Record(&AuditLog{
		ActorEmail: "system",
		ActorRole:  "system",
		AuthMethod: "system",
		Action:     action,
		Method:     "SYSTEM",
		Path:       "/system/siwc-gate/" + email,
		StatusCode: status,
		Extra:      extra,
	})
}
