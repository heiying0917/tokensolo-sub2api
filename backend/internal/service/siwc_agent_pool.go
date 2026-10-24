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

	siwcAgentRoleExtraKey  = "siwc_agent_role"
	siwcAgentSwitchLockTTL = 15 * time.Second
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
	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, email)
	if err != nil {
		return nil, err
	}
	sortSIWCAgents(siblings)
	group := &SIWCAgentGroup{Email: email, Agents: make([]SIWCAgentView, 0, len(siblings))}
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

	siblings, err := listOpenAISIWCSiblings(ctx, s.accountRepo, email)
	if err != nil {
		return nil, err
	}
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
			return nil, err
		}
	}
	if err := s.setRole(ctx, target.ID, SIWCAgentRoleActive, true); err != nil {
		return nil, err
	}
	if s.runtimeBlocker != nil {
		s.runtimeBlocker.ClearAccountSchedulingBlock(target.ID)
	}
	s.recordAudit(actor, "siwc_agent.activate", email, map[string]any{"email": email, "account_id": target.ID})
	return s.List(ctx, target.ID)
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

// ErrSIWCAgentStandbySchedulable 待命 Agent 不允许直接打开调度，须经切换。
var ErrSIWCAgentStandbySchedulable = infraerrors.New(http.StatusConflict, "SIWC_AGENT_STANDBY", "standby siwc agent cannot be made schedulable directly; switch the active agent instead")

// siwcAgentFailoverer 由 SIWCAgentPoolService 实现，注入到凭据失效的汇聚点。
type siwcAgentFailoverer interface {
	Failover(ctx context.Context, dead *Account)
}

var _ siwcAgentFailoverer = (*SIWCAgentPoolService)(nil)
