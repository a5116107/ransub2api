package service

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
)

// cnQuotaProber 抽象额度探测（*CNProviderQuotaService 实现，测试可替换）。
type cnQuotaProber interface {
	QueryUsage(ctx context.Context, accountID int64) (*CNProviderQuotaProbeResult, error)
}

// cnQuotaProbeConcurrency bounds all periodic checks; payg keeps one in-flight.
const cnQuotaProbeConcurrency = 4

// CNProviderBalanceCheckService 周期性探测国产供应商账号：
//   - payg（按量付费）：余额低于阈值则临时停调，恢复则清除（仅清除本服务写入的停调）；
//   - coding plan：调用 CNProviderQuotaService 探测 5h/weekly 滚动窗口并落 extra 快照，
//     调度阈值评估（cnProviderThresholdCandidates）据此自动停调/恢复。
//
// 克隆自 AccountExpiryService 的 Start/Stop/runOnce + ticker 骨架。
// 余额探测仅覆盖有公开余额端点的 kimi / deepseek；智谱无余额端点，仅靠响应式 429/402。
// 额度探测覆盖 kimi / zhipu 的 coding plan 账号（deepseek 无 coding 套餐）。
// Command Code 账号一次探测同时刷新窗口快照与积分余额，积分低于阈值时同样临时停调。
// Cline 账号同样一次探测刷新 ClinePass 窗口与积分，只冷却用完的一边（见 cline_usage.go）。
type CNProviderBalanceCheckService struct {
	accountRepo      AccountRepository
	balanceService   *CNProviderBalanceService
	quotaService     cnQuotaProber
	monitoringBudget *MonitoringProbeBudgetService
	cfg              *config.Config
	interval         time.Duration
	lifecycleMu      sync.Mutex
	started          bool
	stopped          bool
	drained          bool
	instanceID       string
	lastScan         *CNMonitoringInventory
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	cycleMu          sync.Mutex
	turnSequence     uint64
	turns            map[int64]cnBalanceProbeTurn
}

// NewCNProviderBalanceCheckService 构造周期余额/额度检测服务。
// interval <= 0 时 Start() 直接返回（不启动），便于通过配置关闭。
func NewCNProviderBalanceCheckService(
	accountRepo AccountRepository,
	balanceService *CNProviderBalanceService,
	quotaService *CNProviderQuotaService,
	cfg *config.Config,
	interval time.Duration,
) *CNProviderBalanceCheckService {
	return &CNProviderBalanceCheckService{
		accountRepo:    accountRepo,
		balanceService: balanceService,
		quotaService:   quotaService,
		cfg:            cfg,
		interval:       interval,
		instanceID:     uuid.NewString(),
	}
}

func (s *CNProviderBalanceCheckService) SetMonitoringBudget(budget *MonitoringProbeBudgetService) {
	s.monitoringBudget = budget
}

// Give every account its own local error state. Detached provider flights keep
// that policy, while ordinary manual quota queries remain unmetered.
func (s *CNProviderBalanceCheckService) probeContext(ctx context.Context) context.Context {
	// The dispatcher owns a fresh state per job. Its adapter must retain that
	// state so local deferrals and admitted physical work reach the queue.
	if probe, ok := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext); ok && probe.accountTest && probe.service == s.monitoringBudget {
		return withAccountTestMonitoringSite(ctx, "")
	}
	return withAccountTestMonitoringSite(s.monitoringBudget.qualityContext(ctx), "")
}

func logCNMonitoringBudgetDeferral(ctx context.Context, accountID int64, err error) bool {
	denial := monitoringProbeDeferral(err, time.Now())
	if denial == nil {
		return false
	}
	recordMonitoringProbeLocalError(ctx, err)
	slog.Info("cn_provider_monitoring_budget_deferred", "account_id", accountID,
		"reason", denial.Reason, "scope", denial.Scope, "retry_at", denial.RetryAt)
	return true
}

func (s *CNProviderBalanceCheckService) Start() {
	if s == nil || s.accountRepo == nil || s.balanceService == nil || s.cfg == nil ||
		!s.cfg.Gateway.CNProviders.BalanceCheckEnabled || s.interval <= 0 {
		return
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.started || s.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.started = cancel, true
	log.Printf("[CNBalance] started (interval=%s threshold=%.2f)", s.interval, s.cfg.Gateway.CNProviders.BalanceThreshold)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		// Wait one period before the first cycle, preserving startup behavior.
		for {
			select {
			case <-ticker.C:
				s.runOnce(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *CNProviderBalanceCheckService) Stop() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	s.lifecycleMu.Unlock()
	s.wg.Wait()
	s.lifecycleMu.Lock()
	s.drained = true
	s.lifecycleMu.Unlock()
}

// Local scheduling age, not evidence freshness or a distributed reservation.
// A rejected request keeps its age; an admitted physical attempt (including a
// partial adapter chain) consumes a turn, so an expensive chain cannot monopolize
// each renewed budget. tried breaks ties between equally old rejected jobs.
type cnBalanceProbeTurn struct {
	served uint64
	tried  uint64
}

type accountProbeCheck func(ctx context.Context, account *Account, threshold float64) cnBalanceCheckOutcome

type accountProbeTarget struct {
	account *Account
	check   accountProbeCheck
	balance bool
}

func (s *CNProviderBalanceCheckService) runOnce(ctx context.Context) {
	// A ticker is serial, but a concurrent invocation must not duplicate its
	// queue or race the local fairness state.
	if !s.cycleMu.TryLock() {
		return
	}
	defer s.cycleMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	collectCtx, cancelCollect := context.WithTimeout(ctx, 30*time.Second)
	targets, complete := s.collectProbeTargets(collectCtx)
	cancelCollect()
	if ctx.Err() == nil {
		s.observeMonitoringInventory(targets, complete)
	}
	if ctx.Err() != nil || len(targets) == 0 {
		if complete {
			s.turns = nil
		}
		return
	}
	if s.turns == nil {
		s.turns = make(map[int64]cnBalanceProbeTurn, len(targets))
	}
	s.turnSequence++
	present := make(map[int64]bool, len(targets))
	for _, target := range targets {
		id := target.account.ID
		present[id] = true
		if _, exists := s.turns[id]; !exists {
			s.turns[id] = cnBalanceProbeTurn{served: s.turnSequence}
		}
	}
	if complete {
		for id := range s.turns {
			if !present[id] {
				delete(s.turns, id)
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		left, right := s.turns[targets[i].account.ID], s.turns[targets[j].account.ID]
		if left.served != right.served {
			return left.served < right.served
		}
		if left.tried != right.tried {
			return left.tried < right.tried
		}
		return targets[i].account.ID < targets[j].account.ID
	})
	var balances, quotas []int
	for i, target := range targets {
		if target.balance {
			balances = append(balances, i)
		} else {
			quotas = append(quotas, i)
		}
	}
	// Retain the original workload-derived 30..300s bound. This is not a
	// promise that every target fits or that a detached flight was withdrawn.
	batches := (len(quotas) + cnQuotaProbeConcurrency - 1) / cnQuotaProbeConcurrency
	timeout := min(300*time.Second, 30*time.Second+time.Duration(batches)*15*time.Second+time.Duration(len(balances))*5*time.Second)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type finished struct {
		index   int
		outcome cnBalanceCheckOutcome
		served  bool
		stop    bool
	}
	done := make(chan finished, cnQuotaProbeConcurrency)
	threshold := s.cfg.Gateway.CNProviders.BalanceThreshold
	active, nextBalance, nextQuota, paused, cleared := 0, 0, 0, 0, 0
	balanceBusy, exhausted := false, false
	for {
		for !exhausted && runCtx.Err() == nil && active < cnQuotaProbeConcurrency {
			index := -1
			if nextQuota < len(quotas) {
				index = quotas[nextQuota]
			}
			if !balanceBusy && nextBalance < len(balances) && (index < 0 || balances[nextBalance] < index) {
				index = balances[nextBalance]
			}
			if index < 0 {
				break
			}
			target := targets[index]
			if target.balance {
				nextBalance++
				balanceBusy = true
			} else {
				nextQuota++
			}
			s.turnSequence++
			turn := s.turns[target.account.ID]
			turn.tried = s.turnSequence
			s.turns[target.account.ID] = turn
			active++
			// Always create job-owned state, even if the caller already carries
			// this service's policy. Adapters retain it within this job only.
			jobCtx := withAccountTestMonitoringSite(s.monitoringBudget.qualityContext(runCtx), "")
			go func() {
				result := finished{index: index}
				defer func() {
					if recover() != nil {
						log.Printf("[CNBalance] probe account %d panicked", target.account.ID)
					}
					local := monitoringProbeLocalError(jobCtx)
					denial := monitoringProbeDeferral(local, time.Now())
					result.served = denial == nil && jobCtx.Err() == nil
					result.served = result.served || monitoringProbeWasAdmitted(jobCtx)
					if denial != nil {
						switch denial.Scope {
						case "key", "key_minute", "site", "site_minute":
							// Independent credentials/sites may still have capacity.
						default:
							result.stop = true
						}
					}
					done <- result
				}()
				result.outcome = target.check(jobCtx, target.account, threshold)
			}()
		}
		if active == 0 {
			break
		}
		result := <-done
		active--
		target := targets[result.index]
		if target.balance {
			balanceBusy = false
		}
		if result.served {
			s.turnSequence++
			turn := s.turns[target.account.ID]
			turn.served = s.turnSequence
			s.turns[target.account.ID] = turn
		}
		exhausted = exhausted || result.stop
		switch result.outcome {
		case cnBalancePaused:
			paused++
		case cnBalanceCleared:
			cleared++
		}
	}
	if paused > 0 || cleared > 0 {
		log.Printf("[CNBalance] paused=%d cleared=%d (threshold=%.2f)", paused, cleared, threshold)
	}
}

// Collect only original supported active targets. A partial inventory does not
// erase absent scheduling ages; a complete one reclaims removed account state.
func (s *CNProviderBalanceCheckService) collectProbeTargets(ctx context.Context) ([]accountProbeTarget, bool) {
	var targets []accountProbeTarget
	complete := true
	seen := map[int64]bool{}
	for _, platform := range []string{PlatformKimi, PlatformDeepseek, PlatformZhipu, PlatformMiniMax, PlatformCommandCode, PlatformCline, PlatformMistral} {
		if ctx.Err() != nil {
			return targets, false
		}
		if s.quotaService == nil && platform != PlatformKimi && platform != PlatformDeepseek {
			continue
		}
		accounts, err := s.accountRepo.ListByPlatform(ctx, platform)
		if err != nil {
			complete = false
			log.Printf("[CNBalance] list %s accounts failed: %v", platform, err)
			continue
		}
		for i := range accounts {
			account := &accounts[i]
			if !account.IsActive() || seen[account.ID] || IsOllamaCloudUsageAccount(account) {
				continue
			}
			var check accountProbeCheck
			balance := false
			switch {
			case platform == PlatformCommandCode && account.commandCodeUsageSupported() && s.quotaService != nil:
				check = s.checkCommandCode
			case platform == PlatformCline && account.clineAccountAPISupported() && s.quotaService != nil:
				check = s.checkCline
			case platform == PlatformMistral && account.mistralUsageSupported() && s.quotaService != nil:
				check = s.checkMistral
			case account.IsCodingPlan() && s.quotaService != nil && (platform == PlatformKimi || platform == PlatformDeepseek || platform == PlatformZhipu || platform == PlatformMiniMax):
				check = func(ctx context.Context, a *Account, _ float64) cnBalanceCheckOutcome {
					s.probeQuota(ctx, a.ID, a.Platform)
					return cnBalanceNoChange
				}
			case (platform == PlatformKimi || platform == PlatformDeepseek) && !account.IsCodingPlan() && account.Schedulable && s.balanceService != nil:
				check, balance = s.checkOne, true
			}
			if check != nil {
				seen[account.ID] = true
				targets = append(targets, accountProbeTarget{account: account, check: check, balance: balance})
			}
		}
	}
	return targets, complete
}

// checkCommandCode 探测 Command Code 窗口与积分；手动可调度的账号再按积分余额停调 / 恢复。
func (s *CNProviderBalanceCheckService) checkCommandCode(ctx context.Context, account *Account, threshold float64) cnBalanceCheckOutcome {
	ctx = s.probeContext(ctx)
	result, err := s.quotaService.QueryUsage(ctx, account.ID)
	if err != nil {
		if !logCNMonitoringBudgetDeferral(ctx, account.ID, err) {
			log.Printf("[CNBalance] command code usage account %d failed: %v", account.ID, err)
		}
		return cnBalanceNoChange
	}
	if result == nil || !result.Success || result.Balance == nil {
		if result != nil && result.Error != "" {
			log.Printf("[CNBalance] command code usage account %d error: %s", account.ID, result.Error)
		}
		return cnBalanceNoChange
	}
	if !account.Schedulable {
		return cnBalanceNoChange
	}
	return s.applyBalanceResult(ctx, account, result.Balance, threshold)
}

// checkCline 探测 Cline 窗口与积分；钱包冷却在探测中按结果设置或解除，不停整个账号。
func (s *CNProviderBalanceCheckService) checkCline(ctx context.Context, account *Account, _ float64) cnBalanceCheckOutcome {
	ctx = s.probeContext(ctx)
	result, err := s.quotaService.QueryUsage(ctx, account.ID)
	if err != nil {
		if !logCNMonitoringBudgetDeferral(ctx, account.ID, err) {
			log.Printf("[CNBalance] cline usage account %d failed: %v", account.ID, err)
		}
	} else if result != nil && !result.Success && result.Error != "" {
		log.Printf("[CNBalance] cline usage account %d error: %s", account.ID, result.Error)
	}
	return cnBalanceNoChange
}

// checkMistral 探测 Mistral 月度预算窗口与剩余额度；手动可调度的账号再按剩余额度停调 / 恢复。
// 开启按量付费（payg_enabled）的账号不受包月预算余额阈值限制。
func (s *CNProviderBalanceCheckService) checkMistral(ctx context.Context, account *Account, threshold float64) cnBalanceCheckOutcome {
	ctx = s.probeContext(ctx)
	result, err := s.quotaService.QueryUsage(ctx, account.ID)
	if err != nil {
		if !logCNMonitoringBudgetDeferral(ctx, account.ID, err) {
			log.Printf("[CNBalance] mistral usage account %d failed: %v", account.ID, err)
		}
		return cnBalanceNoChange
	}
	if result == nil || !result.Success || result.Balance == nil {
		if result != nil && result.Error != "" {
			log.Printf("[CNBalance] mistral usage account %d error: %s", account.ID, result.Error)
		}
		return cnBalanceNoChange
	}
	if !account.Schedulable {
		return cnBalanceNoChange
	}
	effectiveThreshold := threshold
	if result.Source == mistralQuotaSourcePayg || !mistralWindowsBinding(account.Extra) {
		effectiveThreshold = 0
	}
	return s.applyBalanceResult(ctx, account, result.Balance, effectiveThreshold)
}

// probeQuota 探测单个 coding plan 账号的滚动窗口用量并落 extra 快照。
// 不在此处做停调/恢复决策：调度阈值评估读取快照统一判定（含暂停账号的续停）。
func (s *CNProviderBalanceCheckService) probeQuota(ctx context.Context, accountID int64, platform string) {
	if s.quotaService == nil {
		return
	}
	ctx = s.probeContext(ctx)
	result, err := s.quotaService.QueryUsage(ctx, accountID)
	if err != nil {
		if !logCNMonitoringBudgetDeferral(ctx, accountID, err) {
			log.Printf("[CNBalance] quota probe account %d (%s) failed: %v", accountID, platform, err)
		}
		return
	}
	if result != nil && !result.Success && result.Error != "" {
		log.Printf("[CNBalance] quota probe account %d (%s) error: %s", accountID, platform, result.Error)
	}
}

type cnBalanceCheckOutcome int

const (
	cnBalanceNoChange cnBalanceCheckOutcome = iota
	cnBalancePaused
	cnBalanceCleared
)

// checkOne 探测单账号余额并决定停调/恢复。探测失败时不动现状（避免瞬时网络抖动
// 误解除或误停调）。
func (s *CNProviderBalanceCheckService) checkOne(ctx context.Context, account *Account, threshold float64) cnBalanceCheckOutcome {
	ctx = s.probeContext(ctx)
	result, err := s.balanceService.QueryBalance(ctx, account.ID)
	if err != nil {
		logCNMonitoringBudgetDeferral(ctx, account.ID, err)
	}
	if err != nil || result == nil || !result.Success {
		return cnBalanceNoChange
	}
	return s.applyBalanceResult(ctx, account, result, threshold)
}

// applyBalanceResult 按余额探测结果停调或恢复账号。
func (s *CNProviderBalanceCheckService) applyBalanceResult(ctx context.Context, account *Account, result *CNProviderBalanceResult, threshold float64) cnBalanceCheckOutcome {
	// 双币种（deepseek CNY+USD）任一币种余额达标即可继续调度；仅当全部低于
	// 阈值（或不可用）才停调。
	low := !result.Available || allCNBalancesBelowThreshold(result, threshold)
	if low {
		// 已被（任何来源）停调时不覆盖其 reason。
		if !account.IsSchedulable() {
			return cnBalanceNoChange
		}
		reason := cnBalanceLowReason(fmt.Sprintf("余额 %.4g %s 低于阈值 %.2f", result.Balance, result.Currency, threshold))
		if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, time.Now().Add(s.cooldown()), reason); err != nil {
			log.Printf("[CNBalance] pause account %d failed: %v", account.ID, err)
			return cnBalanceNoChange
		}
		log.Printf("[CNBalance] paused account %d (%s): balance=%.4g %s", account.ID, account.Platform, result.Balance, result.Currency)
		return cnBalancePaused
	}

	// 余额健康：仅清除「本服务写入」的临时停调（reason 前缀匹配），不触碰其他子系统。
	if account.TempUnschedulableUntil != nil && strings.HasPrefix(account.TempUnschedulableReason, cnBalanceLowReasonPrefix) {
		if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
			log.Printf("[CNBalance] clear account %d failed: %v", account.ID, err)
			return cnBalanceNoChange
		}
		log.Printf("[CNBalance] reactivated account %d (%s): balance=%.4g %s", account.ID, account.Platform, result.Balance, result.Currency)
		return cnBalanceCleared
	}
	return cnBalanceNoChange
}

// allCNBalancesBelowThreshold 判断全部币种余额是否均低于阈值。
// 无明细时退回主币种判定（与旧行为一致）。
func allCNBalancesBelowThreshold(result *CNProviderBalanceResult, threshold float64) bool {
	if len(result.Balances) == 0 {
		return result.Balance < threshold
	}
	for _, entry := range result.Balances {
		if entry.Balance >= threshold {
			return false
		}
	}
	return true
}

// cooldown 返回临时停调持续时长（= 2× 检测周期），与响应式 402/429 路径一致。
func (s *CNProviderBalanceCheckService) cooldown() time.Duration {
	return cnBalanceCheckCooldown(s.cfg)
}
