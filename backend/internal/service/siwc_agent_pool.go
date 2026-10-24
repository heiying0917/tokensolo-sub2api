package service

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

// SIWC 多 Agent：同邮箱的 SIWC 行是同一 ChatGPT 账号下的多个 Agent（每份凭据一行）。
// 同一时刻只有一个活跃 Agent 可调度，其余待命（schedulable=false，后台照常刷新令牌）。
// 角色存 extra.siwc_agent_role；缺失视为 active，兼容功能上线前创建的旧行。
const (
	SIWCAgentRoleActive  = "active"
	SIWCAgentRoleStandby = "standby"

	siwcAgentRoleExtraKey = "siwc_agent_role"
	// siwcAgentParentExtraKey 子 Agent 指向主行 ID；主行写 0。管理端账号列表只显示主行。
	siwcAgentParentExtraKey = "siwc_agent_parent_id"
	// siwcAccountPausedExtraKey 写在主行上：整个 ChatGPT 账号被手动暂停（列表开关关闭）。
	siwcAccountPausedExtraKey = "siwc_account_paused"
	siwcAgentSwitchLockTTL    = 15 * time.Second
)

// GetOpenAISIWCAgentRole 返回 SIWC Agent 角色；非 SIWC 账号返回空串。
func (a *Account) GetOpenAISIWCAgentRole() string {
	if a == nil || !a.IsOpenAISIWC() {
		return ""
	}
	if raw, ok := a.Extra[siwcAgentRoleExtraKey].(string); ok && strings.EqualFold(strings.TrimSpace(raw), SIWCAgentRoleStandby) {
		return SIWCAgentRoleStandby
	}
	return SIWCAgentRoleActive
}

// GetOpenAISIWCAgentParentID 返回子 Agent 所属主行 ID；主行或非 SIWC 返回 0。
func (a *Account) GetOpenAISIWCAgentParentID() int64 {
	if a == nil || !a.IsOpenAISIWC() || a.Extra == nil {
		return 0
	}
	return extraInt64Value(a.Extra[siwcAgentParentExtraKey])
}

// IsOpenAISIWCAgentChild 报告账号是否为挂在主行下的子 Agent（列表隐藏）。
func (a *Account) IsOpenAISIWCAgentChild() bool {
	return a.GetOpenAISIWCAgentParentID() > 0
}

type accountListHideSIWCAgentChildrenKey struct{}

// WithAccountListHideSIWCAgentChildren 标记本次账号列表查询隐藏子 Agent。
// 走 ctx 而不改仓储签名：只有管理端账号列表需要隐藏，导出 / 运维统计 / 额度重置等仍要看见全部行。
func WithAccountListHideSIWCAgentChildren(ctx context.Context) context.Context {
	return context.WithValue(ctx, accountListHideSIWCAgentChildrenKey{}, true)
}

// AccountListHidesSIWCAgentChildren 供仓储读取隐藏标记。
func AccountListHidesSIWCAgentChildren(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	hide, _ := ctx.Value(accountListHideSIWCAgentChildrenKey{}).(bool)
	return hide
}

// IsOpenAISIWCStandby 报告账号是否为待命 SIWC Agent。
func (a *Account) IsOpenAISIWCStandby() bool {
	return a.GetOpenAISIWCAgentRole() == SIWCAgentRoleStandby
}

// siwcAgentUsable 判断 Agent 能否接流量：状态正常、未被资格闸门扣住、有 refresh_token。
func siwcAgentUsable(a *Account) bool {
	if a == nil || !a.IsOpenAISIWC() || a.Status != StatusActive {
		return false
	}
	if gate := ReadSIWCGateState(a); gate != nil && gate.IsGuarded() {
		return false
	}
	return strings.TrimSpace(a.GetCredential("refresh_token")) != ""
}

// SIWCAgentView 是 Agent 管理弹窗的一行。
type SIWCAgentView struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	Schedulable bool       `json:"schedulable"`
	Usable      bool       `json:"usable"`
	ErrorMsg    string     `json:"error_message,omitempty"`
	AgentName   string     `json:"agent_name,omitempty"`
	ExpiresAt   string     `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// SIWCAgentGroup 是一个 ChatGPT 账号下的全部 Agent。
type SIWCAgentGroup struct {
	Email                 string          `json:"email"`
	PrimaryAccountID      int64           `json:"primary_account_id"`
	Paused                bool            `json:"paused"`
	ActiveAccountID       int64           `json:"active_account_id"`
	ManagementAccountID   int64           `json:"management_account_id"`
	ManagementAccountName string          `json:"management_account_name,omitempty"`
	Agents                []SIWCAgentView `json:"agents"`
}

// SIWCAgentPoolService 管理 SIWC Agent 的切换与自动故障转移。
type SIWCAgentPoolService struct {
	accountRepo    AccountRepository
	audit          *AuditLogService
	lockCache      LeaderLockCache
	runtimeBlocker AccountRuntimeBlocker
	instanceID     string
}

// NewSIWCAgentPoolService 构造 Agent 池服务。
func NewSIWCAgentPoolService(accountRepo AccountRepository, audit *AuditLogService, lockCache LeaderLockCache, runtimeBlocker AccountRuntimeBlocker) *SIWCAgentPoolService {
	return &SIWCAgentPoolService{
		accountRepo:    accountRepo,
		audit:          audit,
		lockCache:      lockCache,
		runtimeBlocker: runtimeBlocker,
		instanceID:     uuid.NewString(),
	}
}

func sortSIWCAgents(agents []Account) {
	sort.SliceStable(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
}

// List 返回账号所在 Agent 组。
func (s *SIWCAgentPoolService) List(ctx context.Context, accountID int64) (*SIWCAgentGroup, error) {
	account, err := s.loadSIWC(ctx, accountID)
	if err != nil {
		return nil, err
	}
	email := openAIAccountCredentialEmail(account)
	siblings, primaryID, err := s.normalizeGroup(ctx, email)
	if err != nil {
		return nil, err
	}
	group := &SIWCAgentGroup{Email: email, PrimaryAccountID: primaryID, Paused: siwcGroupPaused(siblings), Agents: make([]SIWCAgentView, 0, len(siblings))}
	for i := range siblings {
		sib := &siblings[i]
		role := sib.GetOpenAISIWCAgentRole()
		if role == SIWCAgentRoleActive && group.ActiveAccountID == 0 && siwcAgentUsable(sib) && sib.Schedulable {
			group.ActiveAccountID = sib.ID
		}
		group.Agents = append(group.Agents, SIWCAgentView{
			ID:          sib.ID,
			Name:        sib.Name,
			Role:        role,
			Status:      sib.Status,
			Schedulable: sib.Schedulable,
			Usable:      siwcAgentUsable(sib),
			ErrorMsg:    sib.ErrorMessage,
			AgentName:   sib.GetCredential("siwc_agent_name"),
			ExpiresAt:   sib.GetCredential("expires_at"),
			LastUsedAt:  sib.LastUsedAt,
			CreatedAt:   sib.CreatedAt,
		})
	}
	if mgmt, mgmtErr := resolveOpenAISIWCManagementAccount(ctx, s.accountRepo, account); mgmtErr == nil && mgmt != nil {
		group.ManagementAccountID = mgmt.ID
		group.ManagementAccountName = mgmt.Name
	}
	return group, nil
}

// RoleForNewAgent 决定新建 Agent 的初始角色：组内（除自身外）已有可用活跃 Agent 则待命，否则活跃。
func (s *SIWCAgentPoolService) RoleForNewAgent(ctx context.Context, email string, excludeID int64) string {
	if s == nil {
		return SIWCAgentRoleActive
	}
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, email)
	if err != nil {
		slog.Warn("siwc_agent.role_lookup_failed", "email", email, "error", err)
		return SIWCAgentRoleActive
	}
	for i := range siblings {
		sib := &siblings[i]
		if sib.ID == excludeID {
			continue
		}
		if sib.GetOpenAISIWCAgentRole() == SIWCAgentRoleActive && siwcAgentUsable(sib) {
			return SIWCAgentRoleStandby
		}
	}
	return SIWCAgentRoleActive
}

// AssignNewAgentRole 在新建 / 导入 SIWC 行后落角色：
// 组内已有可用活跃 Agent → 待命且不可调度；否则经 Activate 成为活跃并把其余行收敛为待命。
// 返回最终角色；失败只记日志，不影响建号结果。
func (s *SIWCAgentPoolService) AssignNewAgentRole(ctx context.Context, account *Account) string {
	if s == nil || account == nil || !account.IsOpenAISIWC() {
		return ""
	}
	ctx = context.WithoutCancel(ctx)
	// 回读最新状态：导入路径的强刷可能已把该行置错
	if fresh, err := s.accountRepo.GetByID(ctx, account.ID); err == nil && fresh != nil {
		account = fresh
	}
	role := s.RoleForNewAgent(ctx, openAIAccountCredentialEmail(account), account.ID)
	// 新 Agent 挂到已有主行下（列表只显示主行）
	defer func() {
		if _, _, err := s.normalizeGroup(ctx, openAIAccountCredentialEmail(account)); err != nil {
			slog.Warn("siwc_agent.normalize_after_assign_failed", "account_id", account.ID, "error", err)
		}
	}()
	if role == SIWCAgentRoleStandby {
		if err := s.setRole(ctx, account.ID, SIWCAgentRoleStandby, false); err != nil {
			slog.Warn("siwc_agent.assign_standby_failed", "account_id", account.ID, "error", err)
		}
		return role
	}
	if !siwcAgentUsable(account) {
		// 新行本身不可用（如导入后强刷失败）：只落角色，不抢活跃
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{siwcAgentRoleExtraKey: SIWCAgentRoleStandby}); err != nil {
			slog.Warn("siwc_agent.assign_role_failed", "account_id", account.ID, "error", err)
		}
		return SIWCAgentRoleStandby
	}
	if _, err := s.Activate(ctx, account.ID, "system"); err != nil {
		slog.Warn("siwc_agent.assign_active_failed", "account_id", account.ID, "error", err)
	}
	return SIWCAgentRoleActive
}

// Activate 把指定 Agent 设为活跃，组内其余 Agent 置为待命。组级锁防多副本 / 并发切换交错。
func (s *SIWCAgentPoolService) Activate(ctx context.Context, accountID int64, actor string) (*SIWCAgentGroup, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "SIWC_AGENT_NOT_CONFIGURED", "siwc agent pool is not configured")
	}
	target, err := s.loadSIWC(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !siwcAgentUsable(target) {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_AGENT_NOT_USABLE", "agent is not usable (error status, held by the eligibility gate, or missing refresh token)")
	}
	email := openAIAccountCredentialEmail(target)
	release, err := s.acquireSwitchLock(ctx, email)
	if err != nil {
		return nil, err
	}
	defer release()
	// 管理员手动「设为活跃」视为恢复使用该账号：解除账号级暂停；系统动作（故障转移 / 新建）尊重暂停
	if err := s.activateLocked(ctx, target, email, actor, actor == "system"); err != nil {
		return nil, err
	}
	return s.List(ctx, target.ID)
}

// activateLocked 在组锁内把 target 设为活跃、其余设为待命。
// respectPause=true 且账号已暂停时只调整角色，不打开调度；否则解除暂停并打开 target 调度。
func (s *SIWCAgentPoolService) activateLocked(ctx context.Context, target *Account, email, actor string, respectPause bool) error {
	siblings, primaryID, err := s.normalizeGroup(ctx, email)
	if err != nil {
		return err
	}
	paused := siwcGroupPaused(siblings)
	keepPaused := paused && respectPause
	// 先降级其余 Agent 再提升目标，宁可短暂无活跃也不出现双活跃
	for i := range siblings {
		sib := &siblings[i]
		if sib.ID == target.ID {
			continue
		}
		if sib.GetOpenAISIWCAgentRole() == SIWCAgentRoleStandby && !sib.Schedulable {
			continue
		}
		if err := s.setRole(ctx, sib.ID, SIWCAgentRoleStandby, false); err != nil {
			return err
		}
	}
	if err := s.setRole(ctx, target.ID, SIWCAgentRoleActive, !keepPaused); err != nil {
		return err
	}
	if paused && !keepPaused && primaryID > 0 {
		if err := s.accountRepo.UpdateExtra(ctx, primaryID, map[string]any{siwcAccountPausedExtraKey: false}); err != nil {
			return err
		}
	}
	if !keepPaused && s.runtimeBlocker != nil {
		s.runtimeBlocker.ClearAccountSchedulingBlock(target.ID)
	}
	s.recordAudit(actor, "siwc_agent.activate", email, map[string]any{"email": email, "account_id": target.ID, "paused": keepPaused})
	return nil
}

// siwcGroupPaused 报告 Agent 组所属账号是否被手动暂停（标记在主行上）。
func siwcGroupPaused(siblings []Account) bool {
	for i := range siblings {
		if !siblings[i].IsOpenAISIWCAgentChild() {
			if paused, ok := siblings[i].Extra[siwcAccountPausedExtraKey].(bool); ok {
				return paused
			}
			return false
		}
	}
	return false
}

// SetAccountPaused 暂停 / 恢复整个 ChatGPT 账号（列表上的可调度开关）。
//   - 暂停：主行标记 paused，全部 Agent 关闭调度，角色不变（记住活跃的是哪一个）；
//   - 恢复：解除标记，重新启用原活跃 Agent；它不可用时切到第一个可用 Agent；都不可用返回 409。
func (s *SIWCAgentPoolService) SetAccountPaused(ctx context.Context, accountID int64, paused bool, actor string) (*SIWCAgentGroup, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "SIWC_AGENT_NOT_CONFIGURED", "siwc agent pool is not configured")
	}
	anchor, err := s.loadSIWC(ctx, accountID)
	if err != nil {
		return nil, err
	}
	email := openAIAccountCredentialEmail(anchor)
	release, err := s.acquireSwitchLock(ctx, email)
	if err != nil {
		return nil, err
	}
	defer release()
	siblings, primaryID, err := s.normalizeGroup(ctx, email)
	if err != nil {
		return nil, err
	}
	if paused {
		if err := s.accountRepo.UpdateExtra(ctx, primaryID, map[string]any{siwcAccountPausedExtraKey: true}); err != nil {
			return nil, err
		}
		for i := range siblings {
			if siblings[i].Schedulable {
				if err := s.accountRepo.SetSchedulable(ctx, siblings[i].ID, false); err != nil {
					return nil, err
				}
			}
		}
		s.recordAudit(actor, "siwc_agent.pause", email, map[string]any{"email": email, "primary_account_id": primaryID})
		return s.List(ctx, primaryID)
	}

	var target *Account
	for i := range siblings {
		if siblings[i].GetOpenAISIWCAgentRole() == SIWCAgentRoleActive && siwcAgentUsable(&siblings[i]) {
			target = &siblings[i]
			break
		}
	}
	if target == nil {
		for i := range siblings {
			if siwcAgentUsable(&siblings[i]) {
				target = &siblings[i]
				break
			}
		}
	}
	if target == nil {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_AGENT_NONE_USABLE", "no usable agent for this account; re-authorize or add an agent first")
	}
	if err := s.activateLocked(ctx, target, email, actor, false); err != nil {
		return nil, err
	}
	s.recordAudit(actor, "siwc_agent.resume", email, map[string]any{"email": email, "active_account_id": target.ID})
	return s.List(ctx, primaryID)
}

// Failover 在活跃 Agent 凭据失效后切到下一个可用待命 Agent；待命 Agent 失效不动作。
// 403/429 按 ChatGPT 账号生效，由资格闸门 / 冷却处理，不经这里。
func (s *SIWCAgentPoolService) Failover(ctx context.Context, dead *Account) {
	if s == nil || dead == nil || !dead.IsOpenAISIWC() || dead.IsOpenAISIWCStandby() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	email := openAIAccountCredentialEmail(dead)
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, email)
	if err != nil {
		slog.Warn("siwc_agent.failover_list_failed", "account_id", dead.ID, "error", err)
		return
	}
	sortSIWCAgents(siblings)
	for i := range siblings {
		sib := &siblings[i]
		if sib.ID == dead.ID || !siwcAgentUsable(sib) {
			continue
		}
		if sib.GetOpenAISIWCAgentRole() == SIWCAgentRoleActive && sib.Schedulable {
			return // 已有其他可用活跃 Agent
		}
	}
	for i := range siblings {
		sib := &siblings[i]
		if sib.ID == dead.ID || !siwcAgentUsable(sib) {
			continue
		}
		if _, err := s.Activate(ctx, sib.ID, "system"); err != nil {
			slog.Warn("siwc_agent.failover_activate_failed", "dead_account_id", dead.ID, "candidate_id", sib.ID, "error", err)
			continue
		}
		slog.Warn("siwc_agent.failover", "email", email, "dead_account_id", dead.ID, "active_account_id", sib.ID)
		return
	}
	slog.Warn("siwc_agent.failover_no_standby", "email", email, "dead_account_id", dead.ID)
}

// FailoverByID 供只持有账号 ID 的调用点使用。
func (s *SIWCAgentPoolService) FailoverByID(ctx context.Context, accountID int64) {
	if s == nil || s.accountRepo == nil {
		return
	}
	account, err := s.accountRepo.GetByID(context.WithoutCancel(ctx), accountID)
	if err != nil || account == nil {
		return
	}
	s.Failover(ctx, account)
}

func (s *SIWCAgentPoolService) loadSIWC(ctx context.Context, accountID int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil || !account.IsOpenAISIWC() || account.IsCredentialShadow() {
		return nil, infraerrors.New(http.StatusBadRequest, "SIWC_AGENT_NOT_SIWC", "account is not a siwc agent")
	}
	if openAIAccountCredentialEmail(account) == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "SIWC_AGENT_NO_EMAIL", "siwc agent has no email")
	}
	return account, nil
}

func (s *SIWCAgentPoolService) setRole(ctx context.Context, accountID int64, role string, schedulable bool) error {
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{siwcAgentRoleExtraKey: role}); err != nil {
		return err
	}
	return s.accountRepo.SetSchedulable(ctx, accountID, schedulable)
}

func (s *SIWCAgentPoolService) acquireSwitchLock(ctx context.Context, email string) (func(), error) {
	if s.lockCache == nil {
		return func() {}, nil
	}
	key := "siwc_agent:switch:" + email
	ok, err := s.lockCache.TryAcquireLeaderLock(ctx, key, s.instanceID, siwcAgentSwitchLockTTL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusServiceUnavailable, "SIWC_AGENT_LOCK_UNAVAILABLE", "acquire siwc agent switch lock: %v", err)
	}
	if !ok {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_AGENT_SWITCH_BUSY", "another agent switch for this account is in progress")
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.lockCache.ReleaseLeaderLock(releaseCtx, key, s.instanceID)
	}, nil
}

func (s *SIWCAgentPoolService) recordAudit(actor, action, email string, extra map[string]any) {
	if s == nil || s.audit == nil {
		return
	}
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	s.audit.Record(&AuditLog{
		ActorEmail: actor,
		ActorRole:  "system",
		AuthMethod: "system",
		Action:     "system." + action,
		Method:     "SYSTEM",
		Path:       "/system/siwc-agent/" + email,
		StatusCode: http.StatusOK,
		Extra:      extra,
	})
}

// normalizeGroup 收敛同邮箱 Agent 组的主 / 从关系并返回按 ID 排序的成员与主行 ID：
// 主行 = 现有主行（parent=0）里 ID 最小者，都不是主行时取 ID 最小者；其余行 parent 指向主行。幂等。
func (s *SIWCAgentPoolService) normalizeGroup(ctx context.Context, email string) ([]Account, int64, error) {
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, email)
	if err != nil {
		return nil, 0, err
	}
	sortSIWCAgents(siblings)
	if len(siblings) == 0 {
		return siblings, 0, nil
	}
	primaryIdx := 0
	for i := range siblings {
		if !siblings[i].IsOpenAISIWCAgentChild() {
			primaryIdx = i
			break
		}
	}
	primaryID := siblings[primaryIdx].ID
	for i := range siblings {
		sib := &siblings[i]
		want := primaryID
		if sib.ID == primaryID {
			want = 0
		}
		_, hasKey := sib.Extra[siwcAgentParentExtraKey]
		if hasKey && sib.GetOpenAISIWCAgentParentID() == want {
			continue
		}
		if err := s.accountRepo.UpdateExtra(ctx, sib.ID, map[string]any{siwcAgentParentExtraKey: want}); err != nil {
			return nil, 0, err
		}
		if sib.Extra == nil {
			sib.Extra = map[string]any{}
		}
		sib.Extra[siwcAgentParentExtraKey] = want
	}
	return siblings, primaryID, nil
}

// NormalizeAll 启动时把全部 SIWC Agent 组收敛为主 / 从结构（兼容功能上线前已存在的多行）。
// 写入幂等，多副本同时执行结果一致，无需锁。
func (s *SIWCAgentPoolService) NormalizeAll(ctx context.Context) {
	if s == nil || s.accountRepo == nil {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		slog.Warn("siwc_agent.normalize_all_list_failed", "error", err)
		return
	}
	seen := map[string]struct{}{}
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsOpenAISIWC() || acc.IsCredentialShadow() {
			continue
		}
		email := openAIAccountCredentialEmail(acc)
		if email == "" {
			continue
		}
		if _, ok := seen[email]; ok {
			continue
		}
		seen[email] = struct{}{}
		if _, _, err := s.normalizeGroup(ctx, email); err != nil {
			slog.Warn("siwc_agent.normalize_group_failed", "email", email, "error", err)
		}
	}
}

// PrimaryOf 返回账号所在 Agent 组的主行 ID（非 SIWC 返回 0）。
func (s *SIWCAgentPoolService) PrimaryOf(ctx context.Context, account *Account) int64 {
	if s == nil || account == nil || !account.IsOpenAISIWC() {
		return 0
	}
	if parent := account.GetOpenAISIWCAgentParentID(); parent > 0 {
		return parent
	}
	return account.ID
}

// ChildrenOf 返回主行下的子 Agent（不含主行本身）。
func (s *SIWCAgentPoolService) ChildrenOf(ctx context.Context, primary *Account) ([]Account, error) {
	if s == nil || primary == nil || !primary.IsOpenAISIWC() || primary.IsOpenAISIWCAgentChild() {
		return nil, nil
	}
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, openAIAccountCredentialEmail(primary))
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(siblings))
	for i := range siblings {
		if siblings[i].ID != primary.ID && siblings[i].GetOpenAISIWCAgentParentID() == primary.ID {
			out = append(out, siblings[i])
		}
	}
	return out, nil
}

// RemoveAgent 在 Agent 管理里删除一个 Agent：删子 Agent 直接删；删主行时先把下一个 Agent 升为主行，
// 其余改指向新主行，再删原主行。删的是活跃 Agent 时先切到下一个可用 Agent。
func (s *SIWCAgentPoolService) RemoveAgent(ctx context.Context, accountID int64, deleteFn func(context.Context, int64) error) (*SIWCAgentGroup, error) {
	target, err := s.loadSIWC(ctx, accountID)
	if err != nil {
		return nil, err
	}
	email := openAIAccountCredentialEmail(target)
	siblings, primaryID, err := s.normalizeGroup(ctx, email)
	if err != nil {
		return nil, err
	}
	if len(siblings) <= 1 {
		return nil, infraerrors.New(http.StatusConflict, "SIWC_AGENT_LAST", "cannot remove the last agent; delete the account instead")
	}
	var next *Account
	for i := range siblings {
		if siblings[i].ID != target.ID {
			next = &siblings[i]
			break
		}
	}
	if target.ID == primaryID {
		for i := range siblings {
			sib := &siblings[i]
			if sib.ID == target.ID {
				continue
			}
			want := next.ID
			if sib.ID == next.ID {
				want = 0
			}
			if err := s.accountRepo.UpdateExtra(ctx, sib.ID, map[string]any{siwcAgentParentExtraKey: want}); err != nil {
				return nil, err
			}
		}
		// 原主行先降为子行，删除时不会触发主行级联
		if err := s.accountRepo.UpdateExtra(ctx, target.ID, map[string]any{siwcAgentParentExtraKey: next.ID}); err != nil {
			return nil, err
		}
	}
	wasActive := target.GetOpenAISIWCAgentRole() == SIWCAgentRoleActive && target.Schedulable
	if err := deleteFn(ctx, target.ID); err != nil {
		return nil, err
	}
	if wasActive {
		s.Failover(ctx, target)
	}
	return s.List(ctx, next.ID)
}

// SIWCAgentSummary 是账号列表里 SIWC 主行的 Agent 摘要。
type SIWCAgentSummary struct {
	Count           int     `json:"count"`
	ActiveAccountID int64   `json:"active_account_id"`
	AgentIDs        []int64 `json:"agent_ids"`
	// Paused 账号被手动暂停（列表开关关闭）
	Paused bool `json:"paused"`
	// Active 活跃 Agent 本身，供列表把账号状态按活跃 Agent 展示（不序列化）
	Active *Account `json:"-"`
}

// SummariesFor 为一页账号里的 SIWC 主行汇总 Agent 数与活跃 Agent（一次 ListByPlatform）。
func (s *SIWCAgentPoolService) SummariesFor(ctx context.Context, accounts []Account) map[int64]*SIWCAgentSummary {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	primaries := map[int64]struct{}{}
	for i := range accounts {
		if accounts[i].IsOpenAISIWC() && !accounts[i].IsOpenAISIWCAgentChild() {
			primaries[accounts[i].ID] = struct{}{}
		}
	}
	if len(primaries) == 0 {
		return nil
	}
	all, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		slog.Warn("siwc_agent.summary_list_failed", "error", err)
		return nil
	}
	out := make(map[int64]*SIWCAgentSummary, len(primaries))
	ensure := func(id int64) *SIWCAgentSummary {
		if out[id] == nil {
			out[id] = &SIWCAgentSummary{}
		}
		return out[id]
	}
	sortSIWCAgents(all)
	for i := range all {
		acc := &all[i]
		if !acc.IsOpenAISIWC() || acc.IsCredentialShadow() {
			continue
		}
		primaryID := acc.ID
		if parent := acc.GetOpenAISIWCAgentParentID(); parent > 0 {
			primaryID = parent
		}
		if _, ok := primaries[primaryID]; !ok {
			continue
		}
		summary := ensure(primaryID)
		summary.Count++
		summary.AgentIDs = append(summary.AgentIDs, acc.ID)
		if acc.ID == primaryID {
			if paused, ok := acc.Extra[siwcAccountPausedExtraKey].(bool); ok && paused {
				summary.Paused = true
			}
		}
		// 暂停时活跃 Agent 不可调度，但仍按角色认定"活跃的是哪一个"
		if summary.ActiveAccountID == 0 && acc.GetOpenAISIWCAgentRole() == SIWCAgentRoleActive && siwcAgentUsable(acc) {
			summary.ActiveAccountID = acc.ID
			summary.Active = acc
		}
	}
	return out
}

// ChildrenByPrimary 给定一批账号 ID，返回其中 SIWC 主行 → 子 Agent ID（用于列表用量按主行汇总）。
func (s *SIWCAgentPoolService) ChildrenByPrimary(ctx context.Context, accountIDs []int64) map[int64][]int64 {
	if s == nil || s.accountRepo == nil || len(accountIDs) == 0 {
		return nil
	}
	requested := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		requested[id] = struct{}{}
	}
	all, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		slog.Warn("siwc_agent.children_list_failed", "error", err)
		return nil
	}
	out := map[int64][]int64{}
	for i := range all {
		acc := &all[i]
		parent := acc.GetOpenAISIWCAgentParentID()
		if parent <= 0 {
			continue
		}
		if _, ok := requested[parent]; ok {
			out[parent] = append(out[parent], acc.ID)
		}
	}
	return out
}

// ErrSIWCAgentStandbySchedulable 待命 Agent 不允许直接打开调度，须经切换。
var ErrSIWCAgentStandbySchedulable = infraerrors.New(http.StatusConflict, "SIWC_AGENT_STANDBY", "standby siwc agent cannot be made schedulable directly; switch the active agent instead")

// siwcAgentFailoverer 由 SIWCAgentPoolService 实现，注入到凭据失效的汇聚点。
type siwcAgentFailoverer interface {
	Failover(ctx context.Context, dead *Account)
}

var _ siwcAgentFailoverer = (*SIWCAgentPoolService)(nil)
