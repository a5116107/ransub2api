package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const MonitoringProbeBudgetSettingKey = "monitoring_probe_budget"
const MonitoringProbeBudgetLeaseTTL = 2 * time.Minute

var ErrMonitoringProbeBudgetConflict = infraerrors.Conflict("MONITORING_PROBE_BUDGET_CONFLICT", "monitoring budget changed; reload before saving")
var ErrMonitoringProbeBudgetUnavailable = infraerrors.ServiceUnavailable("MONITORING_PROBE_BUDGET_UNAVAILABLE", "monitoring probe budget unavailable")
var ErrMonitoringProbeBudgetLeaseLost = errors.New("monitoring probe budget lease lost")

// Limits refer to physical transport attempts in PostgreSQL's UTC calendar
// windows, not logical tasks, tokens, monetary cost, or downstream requests.
type MonitoringProbeBudgetSettings struct {
	Enabled                bool  `json:"enabled"`
	Revision               int64 `json:"revision"`
	RequestsPerMinute      int64 `json:"requests_per_minute"`
	RequestsPerDay         int64 `json:"requests_per_day"`
	SiteRequestsPerMinute  int64 `json:"site_requests_per_minute"`
	KeyRequestsPerMinute   int64 `json:"key_requests_per_minute"`
	Concurrency            int   `json:"concurrency"`
	SiteConcurrency        int   `json:"site_concurrency"`
	KeyConcurrency         int   `json:"key_concurrency"`
	RecoveryReservePercent int   `json:"recovery_reserve_percent"`
}

func DefaultMonitoringProbeBudgetSettings() MonitoringProbeBudgetSettings {
	return MonitoringProbeBudgetSettings{RequestsPerMinute: 600, RequestsPerDay: 100000,
		SiteRequestsPerMinute: 600, KeyRequestsPerMinute: 200, Concurrency: 16,
		SiteConcurrency: 4, KeyConcurrency: 2, RecoveryReservePercent: 20}
}

func (s MonitoringProbeBudgetSettings) Validate() error {
	if s.Revision < 0 || s.RequestsPerMinute < 1 || s.RequestsPerMinute > 1000000 ||
		s.RequestsPerDay < 1 || s.RequestsPerDay > 1000000000 ||
		s.SiteRequestsPerMinute < 1 || s.SiteRequestsPerMinute > 1000000 ||
		s.KeyRequestsPerMinute < 1 || s.KeyRequestsPerMinute > 1000000 ||
		s.Concurrency < 1 || s.Concurrency > 1024 || s.SiteConcurrency < 1 || s.SiteConcurrency > 1024 ||
		s.KeyConcurrency < 1 || s.KeyConcurrency > 1024 || s.RecoveryReservePercent < 0 || s.RecoveryReservePercent > 80 {
		return infraerrors.BadRequest("INVALID_MONITORING_PROBE_BUDGET", "invalid monitoring budget limits")
	}
	return nil
}

// Denial is local scheduling information. It must never be persisted as an
// upstream request failure or extend the lifetime of earlier passing evidence.
type MonitoringProbeBudgetDenial struct {
	Reason  string    `json:"reason"`
	Scope   string    `json:"scope"`
	RetryAt time.Time `json:"retry_at"`
}

func (e *MonitoringProbeBudgetDenial) Error() string { return "monitoring probe deferred: " + e.Reason }

type MonitoringProbeBudgetSummary struct {
	ObservedAt     time.Time                     `json:"observed_at"`
	Settings       MonitoringProbeBudgetSettings `json:"settings"`
	MinuteUsed     int64                         `json:"minute_used"`
	MinuteRegular  int64                         `json:"minute_regular"`
	DayUsed        int64                         `json:"day_used"`
	DayRegular     int64                         `json:"day_regular"`
	ActiveRequests int                           `json:"active_requests"`
	ActiveRegular  int                           `json:"active_regular"`
}

type MonitoringProbeBudgetRequest struct {
	SiteHash  string
	KeyHashes []string
	Recovery  bool
}

type MonitoringProbeBudgetRepository interface {
	GetSettings(context.Context) (MonitoringProbeBudgetSettings, error)
	SaveSettings(context.Context, MonitoringProbeBudgetSettings) (MonitoringProbeBudgetSettings, error)
	Summary(context.Context) (*MonitoringProbeBudgetSummary, error)
	Acquire(context.Context, MonitoringProbeBudgetRequest) (string, error)
	Renew(context.Context, string) (bool, error)
	Release(context.Context, string) error
}

type MonitoringProbeBudgetService struct {
	repo MonitoringProbeBudgetRepository
}

func NewMonitoringProbeBudgetService(repo MonitoringProbeBudgetRepository) *MonitoringProbeBudgetService {
	return &MonitoringProbeBudgetService{repo: repo}
}

func (s *MonitoringProbeBudgetService) GetSettings(ctx context.Context) (MonitoringProbeBudgetSettings, error) {
	if s == nil || s.repo == nil {
		return MonitoringProbeBudgetSettings{}, ErrMonitoringProbeBudgetUnavailable
	}
	return s.repo.GetSettings(ctx)
}

func (s *MonitoringProbeBudgetService) SaveSettings(ctx context.Context, settings MonitoringProbeBudgetSettings) (MonitoringProbeBudgetSettings, error) {
	if err := settings.Validate(); err != nil {
		return MonitoringProbeBudgetSettings{}, err
	}
	if s == nil || s.repo == nil {
		return MonitoringProbeBudgetSettings{}, ErrMonitoringProbeBudgetUnavailable
	}
	return s.repo.SaveSettings(ctx, settings)
}

func (s *MonitoringProbeBudgetService) Summary(ctx context.Context) (*MonitoringProbeBudgetSummary, error) {
	if s == nil || s.repo == nil {
		return nil, ErrMonitoringProbeBudgetUnavailable
	}
	return s.repo.Summary(ctx)
}

type monitoringProbeBudgetKey struct{}
type monitoringProbeBudgetContext struct {
	service  *MonitoringProbeBudgetService
	site     string
	recovery bool
}

// Apply only to internally owned probes. Business requests never opt into this
// context through headers, JSON parameters, or downstream credentials.
func (s *MonitoringProbeBudgetService) Context(ctx context.Context, site string, recovery bool) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, monitoringProbeBudgetKey{}, monitoringProbeBudgetContext{s, site, recovery})
}

// BeginMonitoringProbeRequest is called once per physical RoundTrip, after
// credential/capacity checks. The returned finish must run after body closure.
func BeginMonitoringProbeRequest(req *http.Request) (*http.Request, func(), error) {
	if req == nil {
		return req, nil, nil
	}
	probe, ok := req.Context().Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
	if !ok {
		return req, nil, nil
	}
	if probe.service == nil || probe.service.repo == nil {
		return req, nil, ErrMonitoringProbeBudgetUnavailable
	}
	if err := context.Cause(req.Context()); err != nil {
		return req, nil, err
	}
	site, err := CanonicalNewAPISite(probe.site)
	if err != nil || !monitoringProbeSiteContains(site, req.URL) {
		return req, nil, ErrMonitoringProbeBudgetUnavailable
	}
	keys := config.UpstreamCredentialFingerprints(req)
	if len(keys) == 0 || slices.Contains(keys, config.UnverifiedUpstreamCredential) {
		return req, nil, ErrMonitoringProbeBudgetUnavailable
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	started := time.Now()
	acquireCtx, cancelAcquire := context.WithTimeout(req.Context(), 2*time.Second)
	id, err := probe.service.repo.Acquire(acquireCtx, MonitoringProbeBudgetRequest{
		SiteHash: monitoringProbeHash(site), KeyHashes: keys, Recovery: probe.recovery,
	})
	cancelAcquire()
	if err != nil {
		var denial *MonitoringProbeBudgetDenial
		if errors.As(err, &denial) {
			return req, nil, denial
		}
		return req, nil, ErrMonitoringProbeBudgetUnavailable
	}
	if id == "" {
		return req, nil, ErrMonitoringProbeBudgetUnavailable
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	stop := keepRenewedLease(ctx, started, MonitoringProbeBudgetLeaseTTL,
		func(ctx context.Context) (bool, error) { return probe.service.repo.Renew(ctx, id) },
		func() { cancel(ErrMonitoringProbeBudgetLeaseLost) })
	finish := sync.OnceFunc(func() {
		stop()
		cancel(nil)
		ctx, cancelRelease := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelRelease()
		_ = probe.service.repo.Release(ctx, id)
	})
	return req.WithContext(ctx), finish, nil
}

func monitoringProbeSiteContains(site string, target *url.URL) bool {
	base, err := url.Parse(site)
	if err != nil || target == nil {
		return false
	}
	origin := *target
	origin.Path, origin.RawPath, origin.RawQuery, origin.Fragment = "", "", "", ""
	canonicalOrigin, err := CanonicalNewAPISite(origin.String())
	if err != nil || canonicalOrigin != base.Scheme+"://"+base.Host {
		return false
	}
	prefix := strings.TrimRight(base.EscapedPath(), "/")
	return prefix == "" || target.EscapedPath() == prefix || strings.HasPrefix(target.EscapedPath(), prefix+"/")
}

func monitoringProbeHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type monitoringProbeBudgetBody struct {
	io.ReadCloser
	ctx    context.Context
	finish func()
}

// Keep local lease loss recognizable through stream/body wrappers.
func (b *monitoringProbeBudgetBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(context.Cause(b.ctx), ErrMonitoringProbeBudgetLeaseLost) {
		return n, ErrMonitoringProbeBudgetLeaseLost
	}
	return n, err
}

func (b *monitoringProbeBudgetBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func FinishMonitoringProbeResponse(ctx context.Context, response *http.Response, err error, finish func()) error {
	if finish == nil {
		return err
	}
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		finish()
		if errors.Is(context.Cause(ctx), ErrMonitoringProbeBudgetLeaseLost) {
			return ErrMonitoringProbeBudgetLeaseLost
		}
		return err
	}
	response.Body = &monitoringProbeBudgetBody{ReadCloser: response.Body, ctx: ctx, finish: finish}
	return nil
}

func monitoringProbeDeferral(err error, now time.Time) *MonitoringProbeBudgetDenial {
	var denial *MonitoringProbeBudgetDenial
	if errors.As(err, &denial) {
		copy := *denial
		if !copy.RetryAt.After(now) {
			copy.RetryAt = now.Add(time.Minute)
		}
		return &copy
	}
	if errors.Is(err, ErrMonitoringProbeBudgetUnavailable) || errors.Is(err, ErrMonitoringProbeBudgetLeaseLost) {
		return &MonitoringProbeBudgetDenial{Reason: "budget_unavailable", Scope: "system", RetryAt: now.Add(time.Minute)}
	}
	return nil
}

func upstreamBillingProbeRecovery(account *Account) bool {
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	return snapshot != nil && snapshot.Status == UpstreamBillingProbeStatusFailed
}

func upstreamBillingProbeDeferral(account *Account) *MonitoringProbeBudgetDenial {
	if account == nil {
		return nil
	}
	raw, err := json.Marshal(account.Extra[UpstreamBillingProbeDeferralExtraKey])
	if err != nil {
		return nil
	}
	var denial MonitoringProbeBudgetDenial
	if json.Unmarshal(raw, &denial) != nil || denial.RetryAt.IsZero() {
		return nil
	}
	return &denial
}

func upstreamBillingProbeDueAt(account *Account) time.Time {
	var due time.Time
	if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil {
		due = snapshot.NextProbeAt
	}
	if deferred := upstreamBillingProbeDeferral(account); deferred != nil && deferred.RetryAt.After(due) {
		due = deferred.RetryAt
	}
	return due
}
