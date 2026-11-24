package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	// These values live in accounts.extra so PR2 does not require a schema migration.
	UpstreamBillingProbeExtraKey           = "upstream_billing_probe"
	UpstreamBillingProbeDeferralExtraKey   = "upstream_billing_probe_deferral"
	UpstreamBillingProbeEnabledExtraKey    = "upstream_billing_probe_enabled"
	UpstreamBillingRateSyncEnabledExtraKey = "upstream_billing_rate_sync_enabled"

	upstreamBillingProbeDefaultIntervalMinutes = 30
	upstreamBillingProbeMinIntervalMinutes     = 5
	upstreamBillingProbeMaxIntervalMinutes     = 24 * 60
	upstreamBillingProbeCycleInterval          = time.Minute
	upstreamBillingProbeRequestTimeout         = 10 * time.Second
	upstreamBillingProbeMaxBodyBytes           = 64 * 1024
	upstreamBillingProbeMaxPerCycle            = 20
	upstreamBillingProbeConcurrency            = 4
	upstreamBillingProbeMaxDelay               = 24 * time.Hour
	// unsupported 账号的重探间隔倍数：上游不是 sub2api 中转就不会突然长出
	// /v1/sub2api/billing，按常规 interval 重排只会持续占满每周期
	// upstreamBillingProbeMaxPerCycle 个名额。
	upstreamBillingProbeUnsupportedDelayFactor = 8
	upstreamBillingProbeAccountRateScale       = 10000.0
	upstreamBillingProbeLeaderLockKey          = "upstream:billing:probe:leader"
	upstreamBillingProbeLeaderLockTTL          = 2 * time.Minute
)

// UpstreamBillingProbeMaxBatchSize limits one manual batch and one runner cycle.
const UpstreamBillingProbeMaxBatchSize = upstreamBillingProbeMaxPerCycle

// upstreamBillingRateSyncMaxMultiplier bounds the value the automatic
// write-back may push into accounts.rate_multiplier.
//
// No other code path bounds that column from above — admins may type any
// non-negative number and the only ceiling is the DECIMAL(10,4) column itself
// (999999.9999). That ceiling is meaningless as a guard: rate_multiplier
// scales the per-request account cost that feeds quota_used, so a single
// declared 999999 would exhaust any account quota on the first request and
// poison cost reporting. 100 is picked as a deliberately generous bound: it is
// two orders of magnitude above the 1.0 default and far above any plausible
// upstream resale markup, so no legitimate declaration is rejected while an
// absurd or hostile one cannot reach the quota control plane unattended.
// It only constrains the automatic path; manual edits keep their old range.
const upstreamBillingRateSyncMaxMultiplier = 100.0

var (
	errUpstreamBillingProbeLeaderLockLost = errors.New("upstream billing probe leader lock lost")
	ErrUpstreamBillingProbeUnavailable    = infraerrors.ServiceUnavailable(
		"UPSTREAM_BILLING_PROBE_UNAVAILABLE", "upstream billing probe is unavailable",
	)
	ErrUpstreamBillingProbeAccountInvalid = infraerrors.BadRequest(
		"UPSTREAM_BILLING_PROBE_ACCOUNT_INVALID", "account is not an API key account",
	)
	ErrUpstreamBillingProbeIdentityChanged = infraerrors.Conflict(
		"UPSTREAM_BILLING_PROBE_IDENTITY_CHANGED", "account identity changed during upstream billing probe; retry the probe",
	)
	ErrUpstreamBillingRateSyncBulkConflict = infraerrors.Conflict(
		"UPSTREAM_BILLING_RATE_SYNC_BULK_CONFLICT",
		"account rate multiplier cannot be changed in bulk while upstream billing rate sync is enabled",
	)
	ErrUpstreamBillingRateSyncConflict = infraerrors.Conflict(
		"UPSTREAM_BILLING_RATE_SYNC_CONFLICT",
		"account rate multiplier cannot be changed while upstream billing rate sync is enabled",
	)
)

const (
	UpstreamBillingProbeStatusOK          = "ok"
	UpstreamBillingProbeStatusUnsupported = "unsupported"
	UpstreamBillingProbeStatusFailed      = "failed"
)

// UpstreamBillingProbeSettings controls the periodic probe runner.
type UpstreamBillingProbeSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// UpstreamBillingProbeSnapshot is persisted in accounts.extra. Data is kept as
// a sanitized map so future response fields do not require a database change.
type UpstreamBillingProbeSnapshot struct {
	Status        string         `json:"status"`
	Data          map[string]any `json:"data,omitempty"`
	ReceivedAt    *time.Time     `json:"received_at,omitempty"`
	FreshUntil    *time.Time     `json:"fresh_until,omitempty"`
	LastAttemptAt time.Time      `json:"last_attempt_at"`
	NextProbeAt   time.Time      `json:"next_probe_at"`
	FailureCount  int            `json:"failure_count,omitempty"`
	HTTPStatus    int            `json:"http_status,omitempty"`
	LastError     string         `json:"last_error,omitempty"`
	// SyncedRateMultiplier records the value this probe wrote into
	// accounts.rate_multiplier. It is only set when the account opted into rate
	// sync and the declared value passed the write-back range check, so the
	// stored snapshot always answers "did this probe move the account rate, and
	// to what" without a separate history table.
	SyncedRateMultiplier *float64 `json:"synced_rate_multiplier,omitempty"`
	// Balance is the upstream balance queried by the same probe. The top-level
	// status, failure count and schedule describe the rate request only.
	Balance *UpstreamBalanceSnapshot `json:"balance,omitempty"`
}

// UpstreamBillingProbeResult is returned by manual probe endpoints.
type UpstreamBillingProbeResult struct {
	AccountID int64                         `json:"account_id"`
	Snapshot  *UpstreamBillingProbeSnapshot `json:"snapshot,omitempty"`
	Deferred  *MonitoringProbeBudgetDenial  `json:"deferred,omitempty"`
	Error     string                        `json:"error,omitempty"`
}

// UpstreamBillingRateSnapshotItem is the compact representation used by the
// account table's background refresh. It intentionally excludes credentials,
// runtime counters, and usage data from the response.
type UpstreamBillingRateSnapshotItem struct {
	Deferred            *MonitoringProbeBudgetDenial  `json:"deferred"`
	ManualApplicability map[string]any                `json:"manual_applicability"`
	CostMultiplier      float64                       `json:"cost_multiplier"`
	AccountID           int64                         `json:"account_id"`
	Snapshot            *UpstreamBillingProbeSnapshot `json:"snapshot"`
}

// BuildUpstreamBillingRateSnapshotItems projects account rows into the
// read-only payload used by the rate refresh endpoint. Decode snapshots here
// so malformed or legacy extra data is handled consistently with probe logic.
func BuildUpstreamBillingRateSnapshotItems(accounts []Account) []UpstreamBillingRateSnapshotItem {
	items := make([]UpstreamBillingRateSnapshotItem, 0, len(accounts))
	for _, account := range accounts {
		var snapshot *UpstreamBillingProbeSnapshot
		// The billing endpoint is supported by every API-key platform; limiting
		// this projection to OpenAI would make the background refresh erase the
		// other platforms' persisted snapshots from the table.
		if account.Type == AccountTypeAPIKey {
			snapshot = decodeUpstreamBillingProbeSnapshot(account.Extra)
		}
		items = append(items, UpstreamBillingRateSnapshotItem{
			Deferred:            upstreamBillingProbeDeferral(&account),
			AccountID:           account.ID,
			CostMultiplier:      account.CostMultiplier(),
			Snapshot:            snapshot,
			ManualApplicability: UpstreamBillingManualApplicabilityView(&account, time.Now()),
		})
	}
	return items
}

type upstreamBillingProbeResponse struct {
	Object                  string   `json:"object"`
	SchemaVersion           int      `json:"schema_version"`
	BillingScope            string   `json:"billing_scope"`
	GroupRateMultiplier     *float64 `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64 `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  *float64 `json:"resolved_rate_multiplier"`
	PeakRateEnabled         *bool    `json:"peak_rate_enabled"`
	PeakStart               *string  `json:"peak_start"`
	PeakEnd                 *string  `json:"peak_end"`
	PeakRateMultiplier      *float64 `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier   *float64 `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier *float64 `json:"effective_rate_multiplier"`
	Timezone                *string  `json:"timezone"`
	ObservedAt              string   `json:"observed_at"`
	SelectedGroupID         *int64   `json:"selected_group_id"`
}

// GetUpstreamBillingProbeSettings returns defaults when the setting is absent.
func (s *SettingService) GetUpstreamBillingProbeSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	defaults := defaultUpstreamBillingProbeSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyUpstreamBillingProbeSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get upstream billing probe settings: %w", err)
	}
	return ParseUpstreamBillingProbeSettings(value)
}

// ParseUpstreamBillingProbeSettings preserves the original missing/legacy
// defaults for both the runner and its read-only capacity compiler.
func ParseUpstreamBillingProbeSettings(value string) (*UpstreamBillingProbeSettings, error) {
	defaults := defaultUpstreamBillingProbeSettings()
	if strings.TrimSpace(value) == "" {
		return defaults, nil
	}
	settings := *defaults
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("parse upstream billing probe settings: %w", err)
	}
	if settings.IntervalMinutes == 0 {
		settings.IntervalMinutes = defaults.IntervalMinutes
	}
	normalizeUpstreamBillingProbeSettings(&settings)
	return &settings, nil
}

// SetUpstreamBillingProbeSettings validates and persists the runner settings.
func (s *SettingService) SetUpstreamBillingProbeSettings(ctx context.Context, settings *UpstreamBillingProbeSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is unavailable")
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal upstream billing probe settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyUpstreamBillingProbeSettings, string(data))
}

func (settings *UpstreamBillingProbeSettings) Validate() error {
	if settings == nil {
		return infraerrors.BadRequest("INVALID_UPSTREAM_BILLING_PROBE_SETTINGS", "settings cannot be nil")
	}
	if settings.IntervalMinutes < upstreamBillingProbeMinIntervalMinutes || settings.IntervalMinutes > upstreamBillingProbeMaxIntervalMinutes {
		return infraerrors.BadRequest(
			"INVALID_UPSTREAM_BILLING_PROBE_INTERVAL",
			fmt.Sprintf("interval_minutes must be between %d and %d", upstreamBillingProbeMinIntervalMinutes, upstreamBillingProbeMaxIntervalMinutes),
		)
	}
	return nil
}

func defaultUpstreamBillingProbeSettings() *UpstreamBillingProbeSettings {
	return &UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: upstreamBillingProbeDefaultIntervalMinutes}
}

func normalizeUpstreamBillingProbeSettings(settings *UpstreamBillingProbeSettings) {
	if settings.IntervalMinutes < upstreamBillingProbeMinIntervalMinutes {
		settings.IntervalMinutes = upstreamBillingProbeMinIntervalMinutes
	}
	if settings.IntervalMinutes > upstreamBillingProbeMaxIntervalMinutes {
		settings.IntervalMinutes = upstreamBillingProbeMaxIntervalMinutes
	}
}

// UpstreamBillingProbeService discovers a remote Sub2API billing snapshot.
type UpstreamBillingProbeService struct {
	monitoringBudget   *MonitoringProbeBudgetService
	newAPIRepo         NewAPIAuthorizationRepository
	newAPIEncryptor    SecretEncryptor
	newAPIFixedKey     bool
	accountRepo        AccountRepository
	accountTestService *AccountTestService
	settingService     *SettingService

	parentCtx    context.Context
	parentCancel context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	started      bool
	stopped      bool
	cycleMu      sync.Mutex
	probeGroup   singleflight.Group
	probeSlots   chan struct{}
	now          func() time.Time
	lockCache    LeaderLockCache
	db           *sql.DB
	instanceID   string
}

type upstreamBillingProbeSnapshotWriter interface {
	UpdateUpstreamBillingProbeSnapshot(context.Context, *Account, *UpstreamBillingProbeSnapshot, *float64) error
}

type upstreamBillingProbeDueAccountLister interface {
	ListDueUpstreamBillingProbeAccounts(context.Context, time.Time, int) ([]Account, error)
}

type upstreamBillingProbeDeferralWriter interface {
	DeferUpstreamBillingProbe(context.Context, *Account, *MonitoringProbeBudgetDenial) error
}

func (s *UpstreamBillingProbeService) SetMonitoringBudget(budget *MonitoringProbeBudgetService) {
	s.monitoringBudget = budget
}

func (s *UpstreamBillingProbeService) MonitoringBudget() *MonitoringProbeBudgetService {
	if s == nil {
		return nil
	}
	return s.monitoringBudget
}

func NewUpstreamBillingProbeService(
	accountRepo AccountRepository,
	accountTestService *AccountTestService,
	settingService *SettingService,
) *UpstreamBillingProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	return &UpstreamBillingProbeService{
		accountRepo:        accountRepo,
		accountTestService: accountTestService,
		settingService:     settingService,
		parentCtx:          ctx,
		parentCancel:       cancel,
		probeSlots:         make(chan struct{}, upstreamBillingProbeConcurrency),
		now:                time.Now,
		instanceID:         uuid.NewString(),
	}
}

func (s *UpstreamBillingProbeService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// ProvideUpstreamBillingProbeService starts the process-wide periodic runner.
func ProvideUpstreamBillingProbeService(
	accountRepo AccountRepository,
	accountTestService *AccountTestService,
	settingService *SettingService,
	lockCache LeaderLockCache,
	db *sql.DB,
	cfg *config.Config,
	newAPIRepo NewAPIAuthorizationRepository,
	encryptor SecretEncryptor,
	budgetRepo MonitoringProbeBudgetRepository,
	cnQuotaCycle *CNProviderBalanceCheckService,
	ollamaUsage *OllamaCloudUsageService,
	groupMonitor *ChannelMonitorV2Service,
	idleProber *ChannelMonitorV3Prober,
) *UpstreamBillingProbeService {
	svc := NewUpstreamBillingProbeService(accountRepo, accountTestService, settingService)
	svc.SetLeaderLock(lockCache, db)
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budgetRepo))
	svc.MonitoringBudget().SetCNQuotaCycle(cnQuotaCycle)
	svc.MonitoringBudget().SetOllamaUsage(ollamaUsage)
	svc.MonitoringBudget().SetResponsesCapability(accountTestService)
	svc.MonitoringBudget().SetGroupProbeQueues(groupMonitor, idleProber)
	svc.SetNewAPIAuthorization(newAPIRepo, encryptor, cfg != nil && cfg.Totp.EncryptionKeyConfigured)
	startBackgroundService(cfg, svc)
	return svc
}

func (s *UpstreamBillingProbeService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *UpstreamBillingProbeService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		s.parentCancel()
	}
	s.mu.Unlock()
	// Every caller waits for the same automatic work, including externally
	// invoked batches; a repeated Stop is not proof that the drain finished.
	s.wg.Wait()
}

func (s *UpstreamBillingProbeService) runLoop() {
	defer s.wg.Done()
	_ = s.RunDue(s.parentCtx)
	ticker := time.NewTicker(upstreamBillingProbeCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			if err := s.RunDue(s.parentCtx); err != nil && s.parentCtx.Err() == nil {
				logger.LegacyPrintf("service.upstream_billing_probe", "run_due_failed: err=%v", err)
			}
		}
	}
}

// RunDue executes at most one bounded batch of due accounts.
func (s *UpstreamBillingProbeService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	// Admission and Stop share the lock so Wait cannot race a new batch added
	// to an empty group. Manual ProbeAccount keeps its own caller lifetime.
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return context.Canceled
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	ctx, cancelRun := context.WithCancelCause(ctx)
	stopParent := context.AfterFunc(s.parentCtx, func() { cancelRun(context.Cause(s.parentCtx)) })
	defer stopParent()
	defer cancelRun(nil)
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()
	if err := context.Cause(s.parentCtx); err != nil {
		cancelRun(err)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}

	settings, err := s.getSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	// Each batch owns a distinct token, including successive batches on the same
	// process. A delayed renewal or release must not affect a successor batch.
	owner := s.instanceID + ":" + uuid.NewString()
	lockStarted := time.Now()
	runRelease, acquired, lockErr := tryAcquireDetectionLeaderLock(ctx, s.lockCache, s.db, upstreamBillingProbeLeaderLockKey, owner, upstreamBillingProbeLeaderLockTTL)
	if lockErr != nil {
		return fmt.Errorf("acquire upstream billing probe leader lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer runRelease()
	if s.lockCache != nil {
		stopRenewal := keepRenewedLease(ctx, lockStarted, upstreamBillingProbeLeaderLockTTL,
			func(ctx context.Context) (bool, error) {
				return s.lockCache.RenewLeaderLock(ctx, upstreamBillingProbeLeaderLockKey, owner, upstreamBillingProbeLeaderLockTTL)
			},
			func() { cancelRun(errUpstreamBillingProbeLeaderLockLost) })
		defer stopRenewal()
	}

	lockNow := time.Now()
	cadenceRelease, acquired, lockErr := tryAcquireDetectionLeaderLock(ctx, s.lockCache, s.db, upstreamBillingProbeLeaderLockKeyAt(lockNow), owner, upstreamBillingProbeLeaderLockTTL)
	if lockErr != nil {
		return fmt.Errorf("acquire upstream billing probe cadence lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer releaseUpstreamBillingProbeLeaderLock(cadenceRelease, lockNow.Truncate(upstreamBillingProbeCycleInterval).Add(upstreamBillingProbeCycleInterval))

	now := s.currentTime()
	accounts, err := s.listDueAccounts(ctx, now)
	if err != nil {
		return fmt.Errorf("list enabled upstream billing probes: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	priorities := make(map[int64]upstreamBillingProbeQueuePriority, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !isUpstreamBillingProbeAccount(&account) || !account.IsActive() || !upstreamBillingProbeEnabled(&account) {
			continue
		}
		if now.Before(upstreamBillingProbeDueAt(&account)) {
			continue
		}
		due = append(due, account)
		priorities[account.ID] = upstreamBillingProbePriority(&account, now)
	}
	sort.SliceStable(due, func(i, j int) bool {
		left, right := priorities[due[i].ID], priorities[due[j].ID]
		if left.recheck != right.recheck {
			return left.recheck
		}
		if !left.urgentAt.Equal(right.urgentAt) {
			return left.urgentAt.Before(right.urgentAt)
		}
		if !left.dueAt.Equal(right.dueAt) {
			return left.dueAt.Before(right.dueAt)
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > upstreamBillingProbeMaxPerCycle {
		due = due[:upstreamBillingProbeMaxPerCycle]
	}

	// Required reviews must finish before exploration competes for scarce
	// physical quota. Bound dispatch as well as the existing shared probe slots.
	split := sort.Search(len(due), func(i int) bool { return !priorities[due[i].ID].recheck })
	for _, phase := range [][]Account{due[:split], due[split:]} {
		var group errgroup.Group
		group.SetLimit(upstreamBillingProbeConcurrency)
		for i := range phase {
			if ctx.Err() != nil {
				break
			}
			accountID := phase[i].ID
			group.Go(func() error {
				if _, probeErr := s.probeScheduledAccount(ctx, accountID, settings.IntervalMinutes); probeErr != nil && ctx.Err() == nil {
					if monitoringProbeDeferral(probeErr, s.currentTime()) != nil {
						return nil
					}
					logger.LegacyPrintf("service.upstream_billing_probe", "probe_due_failed: account_id=%d err=%v", accountID, probeErr)
				}
				return nil
			})
		}
		_ = group.Wait()
	}
	return context.Cause(ctx)
}

type upstreamBillingProbeQueuePriority struct {
	recheck         bool
	urgentAt, dueAt time.Time
}

var upstreamBillingProbeTimestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)

func upstreamBillingProbeTimestamp(raw json.RawMessage) time.Time {
	var value string
	if json.Unmarshal(raw, &value) != nil || !upstreamBillingProbeTimestampPattern.MatchString(value) {
		return time.Time{}
	}
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || stamp.Year() < 1 {
		return time.Time{}
	}
	return stamp.UTC()
}

// Scheduling metadata is not a health verdict. Decode fields independently:
// a damaged evidence timestamp must not erase a saved future retry window.
func upstreamBillingProbeScheduleMetadata(account *Account) (status string, next, received, fresh time.Time) {
	if account == nil {
		return
	}
	raw, err := json.Marshal(account.Extra[UpstreamBillingProbeExtraKey])
	if err != nil {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["status"], &status) != nil || !isUpstreamBillingProbeStatus(status) {
		return "", time.Time{}, time.Time{}, time.Time{}
	}
	return status, upstreamBillingProbeTimestamp(fields["next_probe_at"]), upstreamBillingProbeTimestamp(fields["received_at"]), upstreamBillingProbeTimestamp(fields["fresh_until"])
}

func upstreamBillingProbePriority(account *Account, now time.Time) upstreamBillingProbeQueuePriority {
	status, next, received, fresh := upstreamBillingProbeScheduleMetadata(account)
	received, fresh = received.Truncate(time.Microsecond), fresh.Truncate(time.Microsecond)
	due := upstreamBillingProbeDueAt(account)
	if due.IsZero() {
		due = account.CreatedAt
	}
	priority := upstreamBillingProbeQueuePriority{
		recheck: (status == UpstreamBillingProbeStatusOK || status == UpstreamBillingProbeStatusFailed) && !next.Truncate(time.Microsecond).IsZero(),
		dueAt:   due.Truncate(time.Microsecond), urgentAt: due.Truncate(time.Microsecond),
	}
	// Only a complete, ordered observed evidence window can supply urgency.
	// Unknown clocks retain due age rather than inventing expiry or success.
	if priority.recheck && !received.IsZero() && !fresh.IsZero() && !received.After(now.Truncate(time.Microsecond)) && fresh.After(received) {
		priority.urgentAt = fresh.Truncate(time.Microsecond)
	}
	return priority
}

func (s *UpstreamBillingProbeService) listDueAccounts(ctx context.Context, now time.Time) ([]Account, error) {
	if lister, ok := s.accountRepo.(upstreamBillingProbeDueAccountLister); ok {
		return lister.ListDueUpstreamBillingProbeAccounts(ctx, now, upstreamBillingProbeMaxPerCycle)
	}
	// Non-production repositories and older adapters keep the generic path. The
	// runner still truncates before issuing network requests.
	return s.accountRepo.FindByExtraField(ctx, UpstreamBillingProbeEnabledExtraKey, true)
}

func (s *UpstreamBillingProbeService) getSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	if s.settingService == nil {
		return defaultUpstreamBillingProbeSettings(), nil
	}
	return s.settingService.GetUpstreamBillingProbeSettings(ctx)
}

func (s *UpstreamBillingProbeService) GetSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	return s.getSettings(ctx)
}

func (s *UpstreamBillingProbeService) UpdateSettings(ctx context.Context, settings *UpstreamBillingProbeSettings) error {
	if s == nil || s.settingService == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	if s.monitoringBudget != nil && s.monitoringBudget.repo != nil {
		return s.monitoringBudget.repo.SaveBillingProbeSettings(ctx, *settings)
	}
	return s.settingService.SetUpstreamBillingProbeSettings(ctx, settings)
}

// ProbeAccount performs one manual or scheduled probe. Manual calls ignore both switches.
func (s *UpstreamBillingProbeService) ProbeAccount(ctx context.Context, accountID int64) (*UpstreamBillingProbeSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	settings, err := s.getSettings(ctx)
	if err != nil {
		return nil, err
	}
	return s.probeAccount(ctx, accountID, settings.IntervalMinutes)
}

func (s *UpstreamBillingProbeService) probeAccount(ctx context.Context, accountID int64, intervalMinutes int) (*UpstreamBillingProbeSnapshot, error) {
	return s.probeAccountWithMode(ctx, accountID, intervalMinutes, false)
}

func (s *UpstreamBillingProbeService) probeScheduledAccount(ctx context.Context, accountID int64, intervalMinutes int) (*UpstreamBillingProbeSnapshot, error) {
	return s.probeAccountWithMode(ctx, accountID, intervalMinutes, true)
}

func (s *UpstreamBillingProbeService) probeAccountWithMode(ctx context.Context, accountID int64, intervalMinutes int, requireEnabled bool) (*UpstreamBillingProbeSnapshot, error) {
	key := strconv.FormatInt(accountID, 10)
	value, err, _ := s.probeGroup.Do(key, func() (any, error) {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		select {
		case s.probeSlots <- struct{}{}:
			defer func() { <-s.probeSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		account, loadErr := s.accountRepo.GetByID(ctx, accountID)
		if loadErr != nil {
			return nil, loadErr
		}
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
		if requireEnabled {
			if !account.IsActive() || !upstreamBillingProbeEnabled(account) {
				return nil, nil
			}
			if s.currentTime().Before(upstreamBillingProbeDueAt(account)) {
				return nil, nil
			}
		}
		snapshot, probeErr := s.probeLoadedAccount(ctx, account, intervalMinutes)
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if denial := monitoringProbeDeferral(probeErr, s.currentTime().UTC()); denial != nil {
			writer, ok := s.accountRepo.(upstreamBillingProbeDeferralWriter)
			if !ok {
				return nil, ErrMonitoringProbeBudgetUnavailable
			}
			persistCtx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
			defer cancel()
			if err := writer.DeferUpstreamBillingProbe(persistCtx, account, denial); err != nil {
				return nil, err
			}
			return nil, denial
		}
		return snapshot, probeErr
	})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	snapshot, ok := value.(*UpstreamBillingProbeSnapshot)
	if !ok {
		return nil, fmt.Errorf("invalid upstream billing probe result")
	}
	return snapshot, nil
}

// ProbeAccounts performs a bounded manual batch with the same concurrency limit as the runner.
func (s *UpstreamBillingProbeService) ProbeAccounts(ctx context.Context, accountIDs []int64) []UpstreamBillingProbeResult {
	if len(accountIDs) > upstreamBillingProbeMaxPerCycle {
		accountIDs = accountIDs[:upstreamBillingProbeMaxPerCycle]
	}
	results := make([]UpstreamBillingProbeResult, len(accountIDs))
	if s == nil || s.accountRepo == nil {
		for i, accountID := range accountIDs {
			results[i] = UpstreamBillingProbeResult{AccountID: accountID, Error: ErrUpstreamBillingProbeUnavailable.Error()}
		}
		return results
	}
	settings, settingsErr := s.getSettings(ctx)
	if settingsErr != nil {
		for i, accountID := range accountIDs {
			results[i] = UpstreamBillingProbeResult{AccountID: accountID, Error: safeProbeError(settingsErr)}
		}
		return results
	}
	var group errgroup.Group
	for i, accountID := range accountIDs {
		i, accountID := i, accountID
		results[i].AccountID = accountID
		group.Go(func() error {
			snapshot, err := s.probeAccount(ctx, accountID, settings.IntervalMinutes)
			if err != nil {
				if errors.As(err, &results[i].Deferred) {
					return nil
				}
				results[i].Error = safeProbeError(err)
				return nil
			}
			results[i].Snapshot = snapshot
			return nil
		})
	}
	_ = group.Wait()
	return results
}

func upstreamBillingProbeLeaderLockKeyAt(now time.Time) string {
	return fmt.Sprintf("%s:%d", upstreamBillingProbeLeaderLockKey, now.Unix()/int64(upstreamBillingProbeCycleInterval/time.Second))
}

func releaseUpstreamBillingProbeLeaderLock(release func(), releaseAt time.Time) {
	delay := time.Until(releaseAt)
	if delay <= 0 {
		release()
		return
	}
	time.AfterFunc(delay, release)
}

func (s *UpstreamBillingProbeService) SetAccountEnabled(ctx context.Context, accountID int64, enabled bool) error {
	if s == nil || s.accountRepo == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if !isUpstreamBillingProbeAccount(account) {
		return ErrUpstreamBillingProbeAccountInvalid
	}
	updates := map[string]any{UpstreamBillingProbeEnabledExtraKey: enabled}
	if !enabled {
		updates[UpstreamBillingRateSyncEnabledExtraKey] = false
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, updates)
}

// Adaptive accounts use the effective Chat Completions base for account-level
// billing observation, including providers that route protocols by model. This
// does not establish that other protocol endpoints share the same billing scope.
func upstreamBillingProbeBaseURL(account *Account) string {
	if account == nil {
		return ""
	}
	if account.IsMultiProtocolAPIKey() && account.IsAdaptiveAPIProtocol() {
		return account.GetOpenAIBaseURL()
	}
	return account.GetCredential("base_url")
}

func (s *UpstreamBillingProbeService) probeLoadedAccount(ctx context.Context, account *Account, intervalMinutes int) (*UpstreamBillingProbeSnapshot, error) {
	now := s.currentTime().UTC()
	if s.newAPIRepo != nil {
		binding, err := s.newAPIRepo.GetBinding(ctx, account.ID)
		if err != nil {
			return nil, newAPIError("storage_unavailable")
		}
		if binding != nil && binding.Fingerprint == NewAPIAccountFingerprint(account) {
			return s.probeNewAPIAccount(ctx, account, binding, intervalMinutes)
		}
	}
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "transport_unavailable", 0)
	}
	// 所有 API-key 平台共用 credentials.api_key；探测基址使用统一的生效地址解析。
	apiKey := account.GetCredential("api_key")
	if apiKey == "" {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "missing_api_key", 0)
	}
	baseURL := upstreamBillingProbeBaseURL(account)
	if account.Platform == PlatformOpenAI {
		if baseURL == "" {
			// 保持官方语义：OpenAI 账号无自定义 base 时探官方域（404 → unsupported）。
			baseURL = "https://api.openai.com"
		}
	} else if upstreamBillingProbeTargetIsOfficialAPI(baseURL) {
		// 其他平台 base_url 为空或指向官方 API 根域（前端创建时会把空值
		// 填成官方默认域，且提供 us-east-1.api.x.ai 等官方区域预设）⇒
		// 必无 /v1/sub2api/billing；不发请求，直接记 unsupported，避免
		// 拿账号 Key 周期性请求官方域的不存在路径。
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "unsupported", 0)
	}
	normalizedBaseURL, err := s.accountTestService.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "invalid_base_url", 0)
	}
	ctx = s.monitoringBudget.Context(ctx, normalizedBaseURL, upstreamBillingProbeRecovery(account))
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "proxy_unavailable", 0)
		}
		if account.Proxy.ID != *account.ProxyID {
			return nil, ErrUpstreamBillingProbeIdentityChanged
		}
		proxyURL = account.Proxy.URL()
	}
	target := upstreamBillingProbeTarget{apiKey: apiKey, proxyURL: proxyURL}
	rateResult := s.fetchUpstreamProbeBody(ctx, account, target,
		buildOpenAIEndpointURL(normalizedBaseURL, "/v1/sub2api/billing"), upstreamBillingProbeMaxBodyBytes)
	if rateResult.budgetErr != nil {
		return nil, rateResult.budgetErr
	}
	previousRate := account.BillingRateMultiplier()
	snapshot, syncRate := upstreamBillingRateSnapshotFromResult(account, intervalMinutes, now, rateResult, previousRate)
	if upstreamBalanceProbeAllowed(normalizedBaseURL, rateResult) {
		balanceResult := s.fetchUpstreamProbeBody(ctx, account, target,
			upstreamBalanceProbeURL(normalizedBaseURL, now), upstreamBalanceProbeMaxBodyBytes)
		if balanceResult.budgetErr != nil {
			return nil, balanceResult.budgetErr
		}
		snapshot.Balance = upstreamBalanceSnapshotFromResult(balanceResult, snapshot.Balance, intervalMinutes, now)
		// Sub2API upstreams that predate /v1/sub2api/billing still answer
		// /v1/usage. The stretched unsupported delay is for upstreams that return
		// nothing useful; stretching one with a readable balance would leave that
		// balance stale for most of every cycle.
		if snapshot.Status == UpstreamBillingProbeStatusUnsupported && snapshot.Balance.Status == UpstreamBillingProbeStatusOK {
			snapshot.NextProbeAt = now.Add(nextProbeDelay(intervalMinutes, retryAfter(rateResult.header, now)))
		}
	}
	if err := s.updateSnapshot(ctx, account, snapshot, syncRate); err != nil {
		return nil, err
	}
	if syncRate != nil && previousRate != *syncRate {
		// 写回是后台任务的裸 SQL，不经过管理端路由，因此不会产生 audit_logs 行。
		// old_rate_multiplier 是本次探测开始时读到的值（写回的 CAS 不比对该列）。
		slog.Info("upstream_billing_rate_sync_applied",
			"source", "upstream_billing_probe",
			"account_id", account.ID,
			"old_rate_multiplier", previousRate,
			"new_rate_multiplier", *syncRate,
		)
	}
	return snapshot, nil
}

// upstreamBillingProbeTarget is the credential and route one probe uses for
// both of its upstream requests.
type upstreamBillingProbeTarget struct {
	apiKey   string
	proxyURL string
}

// upstreamBillingProbeHTTPResult is one upstream GET. failure is set when no
// complete body was read; statusCode and header are kept whenever the
// upstream answered at all.
type upstreamBillingProbeHTTPResult struct {
	budgetErr  error
	statusCode int
	header     http.Header
	body       []byte
	failure    string
}

func (s *UpstreamBillingProbeService) fetchUpstreamProbeBody(
	ctx context.Context,
	account *Account,
	target upstreamBillingProbeTarget,
	requestURL string,
	maxBodyBytes int,
) upstreamBillingProbeHTTPResult {
	if ctx.Err() != nil {
		return upstreamBillingProbeHTTPResult{failure: "request_canceled"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, requestURL, bytes.NewReader(nil))
	if err != nil {
		return upstreamBillingProbeHTTPResult{failure: "request_build_failed"}
	}
	// OpenAI 账号保持官方 openai 传输画像；其他平台探测走默认画像。
	profile := HTTPUpstreamProfileDefault
	if account.Platform == PlatformOpenAI {
		profile = HTTPUpstreamProfileOpenAI
	}
	reqCtx := WithHTTPUpstreamProfile(req.Context(), profile)
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
	applyUpstreamBillingProbeHeaders(req.Header, account, target.apiKey)
	var tlsProfile *tlsfingerprint.Profile
	if s.accountTestService.tlsFPProfileService != nil {
		tlsProfile = s.accountTestService.tlsFPProfileService.ResolveTLSProfile(account)
	}
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, target.proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		if monitoringProbeDeferral(err, s.currentTime()) != nil {
			return upstreamBillingProbeHTTPResult{budgetErr: err}
		}
		return upstreamBillingProbeHTTPResult{failure: "request_failed"}
	}
	if resp == nil || resp.Body == nil {
		return upstreamBillingProbeHTTPResult{failure: "empty_response"}
	}
	defer func() { _ = resp.Body.Close() }()
	result := upstreamBillingProbeHTTPResult{statusCode: resp.StatusCode, header: resp.Header}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(maxBodyBytes)+1))
	if readErr != nil {
		if monitoringProbeDeferral(readErr, s.currentTime()) != nil {
			return upstreamBillingProbeHTTPResult{budgetErr: readErr}
		}
		result.failure = "response_read_failed"
		return result
	}
	if len(body) > maxBodyBytes {
		result.failure = "response_too_large"
		return result
	}
	result.body = body
	return result
}

func applyUpstreamBillingProbeHeaders(headers http.Header, account *Account, apiKey string) {
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(headers)
}

// upstreamBillingRateSnapshotFromResult turns one /v1/sub2api/billing response
// into the snapshot to store, plus the rate to write back when the account
// opted into rate sync. The previous balance is carried over; the caller
// replaces it when the same probe also reads the balance.
func upstreamBillingRateSnapshotFromResult(
	account *Account,
	intervalMinutes int,
	now time.Time,
	result upstreamBillingProbeHTTPResult,
	previousRate float64,
) (*UpstreamBillingProbeSnapshot, *float64) {
	reason := result.failure
	if reason == "" {
		switch {
		case result.statusCode == http.StatusNotFound || result.statusCode == http.StatusMethodNotAllowed:
			reason = "unsupported"
		case result.statusCode < 200 || result.statusCode >= 300:
			reason = "http_error"
		}
	}
	var data map[string]any
	if reason == "" {
		var err error
		if data, err = parseUpstreamBillingProbeResponse(result.body); err != nil {
			reason = "invalid_response"
		} else if !upstreamBillingRequestGroupConfirmed(account, data) {
			reason = "group_selection_unconfirmed"
		} else if previous := decodeUpstreamBillingProbeSnapshot(account.Extra); previous != nil && upstreamBillingApplicabilityReplay(previous.Data, data) {
			reason = "applicability_declaration_replayed"
		}
	}
	if reason != "" {
		return upstreamBillingProbeFailureSnapshot(account, intervalMinutes, now, result.statusCode, reason, retryAfter(result.header, now)), nil
	}
	snapshot := &UpstreamBillingProbeSnapshot{
		Status:        UpstreamBillingProbeStatusOK,
		Data:          data,
		ReceivedAt:    probeTimePtr(now),
		FreshUntil:    probeTimePtr(now.Add(2 * time.Duration(intervalMinutes) * time.Minute)),
		LastAttemptAt: now,
		NextProbeAt:   now.Add(nextProbeDelay(intervalMinutes, 0)),
		HTTPStatus:    result.statusCode,
	}
	if previous := decodeUpstreamBillingProbeSnapshot(account.Extra); previous != nil {
		snapshot.Balance = previous.Balance
	}
	// 账号级值域与精度只在真要写回时才有影响：只观察上游声明、未开启同步的
	// 账号不因声明值不适配 accounts.rate_multiplier 而被记成探测失败并进入
	// 指数退避——探测本身成功了，原始声明照常存进快照供展示。
	var syncRate *float64
	if upstreamBillingRateSyncEnabled(account) && data["billing_mode"] != "multi_group" {
		if value, valid := upstreamBillingProbeSyncRate(data); valid {
			syncRate = &value
			snapshot.SyncedRateMultiplier = &value
		} else {
			declared, _ := resolveAccountExtraNumber(data, "resolved_rate_multiplier")
			slog.Warn("upstream_billing_rate_sync_rejected",
				"source", "upstream_billing_probe",
				"account_id", account.ID,
				"declared_resolved_rate_multiplier", declared,
				"max_rate_multiplier", upstreamBillingRateSyncMaxMultiplier,
				"current_rate_multiplier", previousRate,
			)
		}
	}
	return snapshot, syncRate
}

func (s *UpstreamBillingProbeService) persistProbeFailure(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	reason string,
	retryAfterDuration time.Duration,
) (*UpstreamBillingProbeSnapshot, error) {
	snapshot := upstreamBillingProbeFailureSnapshot(account, intervalMinutes, now, statusCode, reason, retryAfterDuration)
	if err := s.updateSnapshot(ctx, account, snapshot, nil); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func upstreamBillingProbeFailureSnapshot(
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	reason string,
	retryAfterDuration time.Duration,
) *UpstreamBillingProbeSnapshot {
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	failureCount := 1
	if previous != nil {
		failureCount = previous.FailureCount + 1
	}
	status := UpstreamBillingProbeStatusFailed
	delay := nextProbeDelay(intervalMinutes, retryAfterDuration)
	if reason == "unsupported" {
		status = UpstreamBillingProbeStatusUnsupported
		delay = unsupportedProbeDelay(intervalMinutes, retryAfterDuration)
	}
	snapshot := &UpstreamBillingProbeSnapshot{
		Status:        status,
		LastAttemptAt: now,
		NextProbeAt:   now.Add(delay),
		FailureCount:  failureCount,
		HTTPStatus:    statusCode,
		LastError:     reason,
	}
	if previous != nil {
		snapshot.Data = previous.Data
		snapshot.ReceivedAt = previous.ReceivedAt
		snapshot.FreshUntil = previous.FreshUntil
		if snapshot.FreshUntil == nil && previous.Status == UpstreamBillingProbeStatusOK && previous.ReceivedAt != nil {
			snapshot.FreshUntil = probeTimePtr(previous.ReceivedAt.Add(2 * time.Duration(intervalMinutes) * time.Minute))
		}
		snapshot.Balance = previous.Balance
	}
	return snapshot
}

func (s *UpstreamBillingProbeService) updateSnapshot(
	ctx context.Context,
	account *Account,
	snapshot *UpstreamBillingProbeSnapshot,
	rateMultiplier *float64,
) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	writer, ok := s.accountRepo.(upstreamBillingProbeSnapshotWriter)
	if !ok {
		return ErrUpstreamBillingProbeUnavailable
	}
	return writer.UpdateUpstreamBillingProbeSnapshot(ctx, account, snapshot, rateMultiplier)
}

func parseUpstreamBillingProbeResponse(body []byte) (map[string]any, error) {
	var response upstreamBillingProbeResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.SchemaVersion == 2 {
		return parseMultiGroupUpstreamBillingResponse(body)
	}
	if response.Object != "sub2api.key_billing" || response.SchemaVersion != 1 || response.BillingScope != "token" {
		return nil, fmt.Errorf("unexpected billing response schema")
	}
	if response.GroupRateMultiplier == nil || response.ResolvedRateMultiplier == nil ||
		response.PeakRateEnabled == nil || response.EffectiveRateMultiplier == nil {
		return nil, fmt.Errorf("incomplete billing response")
	}
	for _, value := range []float64{
		*response.GroupRateMultiplier,
		*response.ResolvedRateMultiplier,
		*response.EffectiveRateMultiplier,
	} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid billing multiplier")
		}
	}
	if response.UserRateMultiplier != nil && (*response.UserRateMultiplier < 0 || math.IsNaN(*response.UserRateMultiplier) || math.IsInf(*response.UserRateMultiplier, 0)) {
		return nil, fmt.Errorf("invalid user billing multiplier")
	}
	expectedResolved := *response.GroupRateMultiplier
	if response.UserRateMultiplier != nil {
		expectedResolved = *response.UserRateMultiplier
	}
	if !equalBillingMultiplier(*response.ResolvedRateMultiplier, expectedResolved) {
		return nil, fmt.Errorf("inconsistent resolved billing multiplier")
	}
	observedAt, err := time.Parse(time.RFC3339Nano, response.ObservedAt)
	if err != nil || observedAt.IsZero() {
		return nil, fmt.Errorf("invalid observed_at")
	}
	data := map[string]any{
		"object":                    response.Object,
		"schema_version":            response.SchemaVersion,
		"billing_scope":             response.BillingScope,
		"group_rate_multiplier":     *response.GroupRateMultiplier,
		"resolved_rate_multiplier":  *response.ResolvedRateMultiplier,
		"peak_rate_enabled":         *response.PeakRateEnabled,
		"effective_rate_multiplier": *response.EffectiveRateMultiplier,
		"observed_at":               observedAt.UTC().Format(time.RFC3339Nano),
	}
	if response.SelectedGroupID != nil {
		if *response.SelectedGroupID <= 0 {
			return nil, fmt.Errorf("invalid selected billing group")
		}
		// Keep the local evidence exact across JSON map round-trips, including
		// IDs above the integer precision of float64.
		data["selected_group_id"] = strconv.FormatInt(*response.SelectedGroupID, 10)
	}
	if response.UserRateMultiplier != nil {
		data["user_rate_multiplier"] = *response.UserRateMultiplier
	}
	if *response.PeakRateEnabled {
		if response.PeakStart == nil || response.PeakEnd == nil || response.Timezone == nil ||
			response.PeakRateMultiplier == nil || response.AppliedPeakMultiplier == nil ||
			*response.PeakStart == "" || *response.PeakEnd == "" || *response.Timezone == "" ||
			*response.PeakRateMultiplier < 0 || *response.AppliedPeakMultiplier < 0 ||
			math.IsNaN(*response.PeakRateMultiplier) || math.IsInf(*response.PeakRateMultiplier, 0) ||
			math.IsNaN(*response.AppliedPeakMultiplier) || math.IsInf(*response.AppliedPeakMultiplier, 0) {
			return nil, fmt.Errorf("incomplete peak billing response")
		}
		data["peak_start"] = *response.PeakStart
		data["peak_end"] = *response.PeakEnd
		data["peak_rate_multiplier"] = *response.PeakRateMultiplier
		data["applied_peak_multiplier"] = *response.AppliedPeakMultiplier
		data["timezone"] = *response.Timezone
	}
	appliedPeak, ok := upstreamBillingPeakMultiplierAt(data, observedAt)
	if !ok {
		return nil, fmt.Errorf("invalid peak billing response")
	}
	if response.PeakRateEnabled != nil && *response.PeakRateEnabled {
		if !equalBillingMultiplier(*response.AppliedPeakMultiplier, appliedPeak) {
			return nil, fmt.Errorf("inconsistent applied peak multiplier")
		}
	} else if response.AppliedPeakMultiplier != nil && !equalBillingMultiplier(*response.AppliedPeakMultiplier, 1) {
		return nil, fmt.Errorf("inconsistent applied peak multiplier")
	}
	if !equalBillingMultiplier(*response.EffectiveRateMultiplier, *response.ResolvedRateMultiplier*appliedPeak) {
		return nil, fmt.Errorf("inconsistent effective billing multiplier")
	}
	parseUpstreamBillingApplicabilityResponse(body, data, observedAt)
	return data, nil
}

func upstreamBillingRequestGroup(account *Account) (int64, error) {
	header := make(http.Header)
	if value, ok := account.HeaderOverrideValue(strings.ToLower(APIKeyGroupHeader)); ok {
		header.Set(APIKeyGroupHeader, value)
	}
	return ParseAPIKeyRequestGroup(header)
}

// A pin is evidence only when the upstream explicitly confirms that same
// request-scoped group. Older upstreams that ignore the header must not sync
// an arbitrary group's multiplier. Unpinned legacy declarations stay valid.
func upstreamBillingRequestGroupConfirmed(account *Account, data map[string]any) bool {
	requested, err := upstreamBillingRequestGroup(account)
	if err != nil {
		return false
	}
	confirmed, present := data["selected_group_id"]
	if requested == 0 {
		return !present
	}
	return confirmed == strconv.FormatInt(requested, 10)
}

func upstreamBillingRateAt(data map[string]any, now time.Time) (float64, bool) {
	if data["billing_mode"] == "multi_group" {
		return 0, false
	}
	if scope, _ := data["billing_scope"].(string); scope != "token" {
		// A New API group ratio alone is not a token-scope price contract.
		// Its explicit, fixed-group extension is consumed again per final
		// request; this account-only price check never grants scope eligibility.
		if data["provider"] != "new_api" || data["object"] != "new_api.group_billing" || scope != "group" {
			return 0, false
		}
		declaration, present := upstreamBillingApplicabilityFromData(data)
		if !present || declaration == nil || now.Before(declaration.ObservedAt) || !now.Before(declaration.ExpiresAt) {
			return 0, false
		}
	}
	base, ok := resolveAccountExtraNumber(data, "resolved_rate_multiplier")
	if !ok || base < 0 || math.IsNaN(base) || math.IsInf(base, 0) {
		return 0, false
	}
	appliedPeak, ok := upstreamBillingPeakMultiplierAt(data, now)
	if !ok {
		return 0, false
	}
	base *= appliedPeak
	if math.IsNaN(base) || math.IsInf(base, 0) {
		return 0, false
	}
	return base, true
}

// upstreamBillingProbeSyncRate converts the declared multiplier into the value
// the automatic write-back may store in accounts.rate_multiplier, at the
// precision that column supports (DECIMAL(10,4)).
//
// It reads resolved_rate_multiplier, not effective_rate_multiplier: the
// effective value folds in the peak coefficient that happened to apply at the
// instant of the probe, so writing it would freeze one probe cycle's peak (or
// off-peak) factor into a static column, while display and scheduling
// recompute the peak factor for the current time through upstreamBillingRateAt.
//
// The accepted range is deliberately narrower than the column:
//   - 0 is rejected. accountCost multiplies the request cost by this value, so
//     an upstream-declared 0 would stop quota_used from ever growing and every
//     admin-configured account quota and cost alert would silently stop
//     working. Admins may still set 0 by hand; only the automatic path refuses.
//   - anything above upstreamBillingRateSyncMaxMultiplier is rejected.
//
// A rejected declaration leaves the current multiplier untouched; the probe
// still records an OK snapshot carrying the raw declaration for display.
func upstreamBillingProbeSyncRate(data map[string]any) (float64, bool) {
	if data["billing_mode"] == "multi_group" {
		return 0, false
	}
	value, ok := resolveAccountExtraNumber(data, "resolved_rate_multiplier")
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	rounded := math.Round(value*upstreamBillingProbeAccountRateScale) / upstreamBillingProbeAccountRateScale
	if rounded <= 0 || rounded > upstreamBillingRateSyncMaxMultiplier {
		return 0, false
	}
	return rounded, true
}

func upstreamBillingPeakMultiplierAt(data map[string]any, now time.Time) (float64, bool) {
	peakEnabled, ok := data["peak_rate_enabled"].(bool)
	if !ok {
		return 0, false
	}
	if !peakEnabled {
		return 1, true
	}

	start, startOK := data["peak_start"].(string)
	end, endOK := data["peak_end"].(string)
	timezoneName, timezoneOK := data["timezone"].(string)
	peakMultiplier, multiplierOK := resolveAccountExtraNumber(data, "peak_rate_multiplier")
	startMinute, validStart := parseMinutes(start)
	endMinute, validEnd := parseMinutes(end)
	if !startOK || !endOK || !timezoneOK || !multiplierOK || !validStart || !validEnd ||
		startMinute >= endMinute || peakMultiplier < 0 || math.IsNaN(peakMultiplier) || math.IsInf(peakMultiplier, 0) {
		return 0, false
	}
	location, err := time.LoadLocation(timezoneName)
	if err != nil {
		return 0, false
	}

	local := now.In(location)
	minute := local.Hour()*60 + local.Minute()
	if minute >= startMinute && minute < endMinute {
		return peakMultiplier, true
	}
	return 1, true
}

func equalBillingMultiplier(left, right float64) bool {
	if math.IsNaN(left) || math.IsNaN(right) || math.IsInf(left, 0) || math.IsInf(right, 0) {
		return false
	}
	scale := math.Max(1, math.Max(math.Abs(left), math.Abs(right)))
	return math.Abs(left-right) <= 1e-9*scale
}

func decodeUpstreamBillingProbeSnapshot(extra map[string]any) *UpstreamBillingProbeSnapshot {
	if extra == nil {
		return nil
	}
	value, ok := extra[UpstreamBillingProbeExtraKey]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var snapshot UpstreamBillingProbeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Status == "" {
		return nil
	}
	if !isUpstreamBillingProbeStatus(snapshot.Status) {
		return nil
	}
	if snapshot.Balance != nil && !isUpstreamBillingProbeStatus(snapshot.Balance.Status) {
		snapshot.Balance = nil
	}
	return &snapshot
}

func isUpstreamBillingProbeStatus(status string) bool {
	return status == UpstreamBillingProbeStatusOK ||
		status == UpstreamBillingProbeStatusUnsupported ||
		status == UpstreamBillingProbeStatusFailed
}

// IsUpstreamBillingProbeIdentity reports whether an account identity may opt
// in to the upstream billing probe. `/v1/sub2api/billing` is a key-scoped
// sub2api convention shared by the supported API-key platforms (including the
// CN providers, whose official-domain accounts are short-circuited to
// "unsupported" by upstreamBillingProbeTargetIsOfficialAPI).
// Non-sub2api upstreams return 404 and the snapshot records "unsupported".
// Only AccountTypeAPIKey is in scope. OAuth/Bedrock hold no static API key to
// present at all; AccountTypeUpstream (antigravity relay accounts) does carry
// a base_url plus a static api_key, but it is deliberately left out of the
// current supported set. New antigravity relay accounts are created with
// type=apikey by the admin form, so only pre-existing type=upstream rows
// cannot turn the probe on.
func IsUpstreamBillingProbeIdentity(platform, accountType string) bool {
	return accountType == AccountTypeAPIKey && domain.IsConcretePlatform(platform)
}

func isUpstreamBillingProbeAccount(account *Account) bool {
	return account != nil && IsUpstreamBillingProbeIdentity(account.Platform, account.Type)
}

// upstreamBillingProbeOfficialAPIDomains lists the root domains of official
// provider APIs. The create form fills empty base_url values with official
// defaults (and offers official regional presets like us-east-1.api.x.ai),
// so probing them would send the account key to an official API path that
// cannot exist. Matching is by registrable root domain — exact host or any
// subdomain, after stripping the port and a trailing DNS dot — because no
// third-party sub2api relay can live under these domains, while custom
// relays (the only targets that can answer /v1/sub2api/billing) always do
// probe. OpenAI-platform accounts never reach this check: they keep the
// upstream-official behavior of probing api.openai.com.
// ollama.com is a first-class configuration here (Ollama Cloud accounts are
// platform openai/anthropic with base_url https://ollama.com/v1), and it is
// an official provider API just like the rest, so it belongs on this list.
// CN provider domains (moonshot.cn / kimi.com / bigmodel.cn / deepseek.com)
// serve the same role: official APIs that can never host /v1/sub2api/billing,
// so their accounts short-circuit to "unsupported" without a request.
var upstreamBillingProbeOfficialAPIDomains = []string{
	"anthropic.com",
	"googleapis.com",
	"x.ai",
	"grok.com",
	"openai.com",
	"ollama.com",
	"moonshot.cn",
	"kimi.com",
	"bigmodel.cn",
	"deepseek.com",
	"opencode.ai",
	"typesafe.ai",
	"commandcode.ai",
	"cline.bot",
	"mistral.ai",
}

func upstreamBillingProbeTargetIsOfficialAPI(baseURL string) bool {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return true
	}
	for _, domain := range upstreamBillingProbeOfficialAPIDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func upstreamBillingProbeEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	enabled, ok := account.Extra[UpstreamBillingProbeEnabledExtraKey].(bool)
	return ok && enabled
}

// upstreamBillingRateSyncEnabled is the probe-side pre-filter deciding whether
// a rate is even proposed for write-back. It is a necessary condition, not the
// authority: the repository CAS re-checks both switches against the row it
// updates, so a switch flipped between load and write can never sneak a rate in.
func upstreamBillingRateSyncEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	enabled, ok := account.Extra[UpstreamBillingRateSyncEnabledExtraKey].(bool)
	return ok && enabled && upstreamBillingProbeEnabled(account)
}

func (s *UpstreamBillingProbeService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func nextProbeDelay(intervalMinutes int, retryAfterDuration time.Duration) time.Duration {
	if retryAfterDuration < 0 {
		retryAfterDuration = 0
	}
	if retryAfterDuration > upstreamBillingProbeMaxDelay {
		retryAfterDuration = upstreamBillingProbeMaxDelay
	}
	interval := time.Duration(intervalMinutes) * time.Minute
	if interval < upstreamBillingProbeMinIntervalMinutes*time.Minute {
		interval = upstreamBillingProbeMinIntervalMinutes * time.Minute
	}
	if interval > upstreamBillingProbeMaxDelay {
		interval = upstreamBillingProbeMaxDelay
	}
	jitterRange := interval / 5
	if jitterRange > 5*time.Minute {
		jitterRange = 5 * time.Minute
	}
	if jitterRange > 0 {
		interval += time.Duration(rand.Int64N(int64(jitterRange)*2+1)) - jitterRange
	}
	if retryAfterDuration > interval {
		// Retry-After is an explicit upstream instruction; honor it over the
		// local backoff interval, clamped to the same 24h ceiling.
		return retryAfterDuration
	}
	if interval > upstreamBillingProbeMaxDelay {
		return upstreamBillingProbeMaxDelay
	}
	return interval
}

// unsupportedProbeDelay 拉长 unsupported 账号的重探间隔，让无效候选自然退出
// 热队列，不再和真正接入 sub2api 的中转账号抢每周期的探测名额。
// 仍按 upstreamBillingProbeMaxDelay 封顶，保证上游后来接入 sub2api 时最迟一天
// 内会被重新发现；base 本身亦受 24h 封顶保护。
func unsupportedProbeDelay(intervalMinutes int, retryAfterDuration time.Duration) time.Duration {
	base := nextProbeDelay(intervalMinutes, retryAfterDuration)
	if base >= upstreamBillingProbeMaxDelay {
		return base
	}
	stretched := base * upstreamBillingProbeUnsupportedDelayFactor
	if stretched > upstreamBillingProbeMaxDelay {
		return upstreamBillingProbeMaxDelay
	}
	return stretched
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); (err == nil || errors.Is(err, strconv.ErrRange)) && seconds > 0 {
		if seconds > int64((1<<63-1)/time.Second) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := at.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

func probeTimePtr(value time.Time) *time.Time {
	return &value
}

func safeProbeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrUpstreamBillingProbeAccountInvalid) {
		return ErrUpstreamBillingProbeAccountInvalid.Error()
	}
	if errors.Is(err, ErrUpstreamBillingProbeUnavailable) {
		return ErrUpstreamBillingProbeUnavailable.Error()
	}
	return "probe_failed"
}
