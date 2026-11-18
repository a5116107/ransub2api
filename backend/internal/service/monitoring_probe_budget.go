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

// Request limits count physical attempts. Token limits count only returned
// usage at completion in PostgreSQL's UTC windows, never estimated cost.
type MonitoringProbeBudgetSettings struct {
	Enabled                bool  `json:"enabled"`
	Revision               int64 `json:"revision"`
	RequestsPerMinute      int64 `json:"requests_per_minute"`
	RequestsPerDay         int64 `json:"requests_per_day"`
	SiteRequestsPerMinute  int64 `json:"site_requests_per_minute"`
	KeyRequestsPerMinute   int64 `json:"key_requests_per_minute"`
	TokensPerMinute        int64 `json:"tokens_per_minute"`
	TokensPerDay           int64 `json:"tokens_per_day"`
	SiteTokensPerMinute    int64 `json:"site_tokens_per_minute"`
	KeyTokensPerMinute     int64 `json:"key_tokens_per_minute"`
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
		s.TokensPerMinute < 0 || s.TokensPerMinute > 1000000000000 ||
		s.TokensPerDay < 0 || s.TokensPerDay > 1000000000000 ||
		s.SiteTokensPerMinute < 0 || s.SiteTokensPerMinute > 1000000000000 ||
		s.KeyTokensPerMinute < 0 || s.KeyTokensPerMinute > 1000000000000 ||
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
	Coverage       *MonitoringProbeCoverage      `json:"coverage"`
	ObservedAt     time.Time                     `json:"observed_at"`
	Settings       MonitoringProbeBudgetSettings `json:"settings"`
	MinuteUsed     int64                         `json:"minute_used"`
	MinuteRegular  int64                         `json:"minute_regular"`
	DayUsed        int64                         `json:"day_used"`
	DayRegular     int64                         `json:"day_regular"`
	ActiveRequests int                           `json:"active_requests"`
	ActiveRegular  int                           `json:"active_regular"`
	MinuteTokens   int64                         `json:"minute_tokens"`
	DayTokens      int64                         `json:"day_tokens"`
	UsageReported  int64                         `json:"usage_reported"`
	UsageUnknown   int64                         `json:"usage_unknown"`
	UsagePending   int64                         `json:"usage_pending"`
}

type MonitoringProbeBudgetRequest struct {
	SiteHash    string
	KeyHashes   []string
	Recovery    bool
	MeterTokens bool
}

type MonitoringProbeBudgetRepository interface {
	GetSettings(context.Context) (MonitoringProbeBudgetSettings, error)
	SaveSettings(context.Context, MonitoringProbeBudgetSettings) (MonitoringProbeBudgetSettings, error)
	PreviewCoverage(context.Context, MonitoringProbeBudgetSettings) (*MonitoringProbeCoverage, error)
	SaveBillingProbeSettings(context.Context, UpstreamBillingProbeSettings) error
	Summary(context.Context) (*MonitoringProbeBudgetSummary, error)
	Acquire(context.Context, MonitoringProbeBudgetRequest) (string, error)
	Renew(context.Context, string) (bool, error)
	Release(context.Context, string, MonitoringProbeTokenUsage) error
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
	service       *MonitoringProbeBudgetService
	sites         []string
	recovery      bool
	accountTest   bool
	ollamaSession bool
	local         *monitoringProbeBudgetError
}

// Preserve local admission/lease errors across adapters that emit SSE error
// strings. This is request-owned and is never accepted from stored rule JSON.
type monitoringProbeBudgetError struct {
	mu  sync.Mutex
	err error
}

func (e *monitoringProbeBudgetError) record(err error) {
	if e == nil || monitoringProbeDeferral(err, time.Now()) == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil {
		e.err = err
	}
}

func monitoringProbeLocalError(ctx context.Context) error {
	probe, _ := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
	if probe.local == nil {
		return nil
	}
	probe.local.mu.Lock()
	defer probe.local.mu.Unlock()
	return probe.local.err
}

func recordMonitoringProbeLocalError(ctx context.Context, err error) {
	probe, _ := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
	probe.local.record(err)
}

// Detached provider flights retain only internal probe policy, not a waiter's
// lifetime or unrelated request metadata.
func monitoringProbeSharedContext(ctx context.Context) context.Context {
	shared := context.Background()
	if probe, ok := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext); ok {
		shared = context.WithValue(WithHTTPUpstreamRedirectsDisabled(shared), monitoringProbeBudgetKey{}, probe)
	}
	return shared
}

// A monitored request must not join an unmetered manual/admin flight.
func monitoringProbeFlightKey(ctx context.Context, key string) string {
	if _, ok := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext); ok {
		return key + ":monitoring"
	}
	return key
}

// ReadMonitoringProbeResponse settles before publishing quota observations.
// Deferred Close could
// otherwise report success before a lease or settlement failure is known.
func ReadMonitoringProbeResponse(ctx context.Context, response *http.Response, limit int64) ([]byte, error) {
	body, readErr := io.ReadAll(io.LimitReader(response.Body, limit))
	closeErr := response.Body.Close()
	if err := monitoringProbeLocalError(ctx); err != nil {
		return nil, err
	}
	return body, errors.Join(readErr, closeErr)
}

func monitoringProbeActive(ctx context.Context) bool {
	_, ok := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
	return ok
}

type monitoringProbeTransport struct{ base http.RoundTripper }

func (t monitoringProbeTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	req, finish, err := BeginMonitoringProbeRequest(original)
	if err != nil {
		if original.Body != nil {
			_ = original.Body.Close()
		}
		return nil, err
	}
	response, err := t.base.RoundTrip(req)
	err = FinishMonitoringProbeResponse(req.Context(), response, err, finish)
	if local := monitoringProbeLocalError(req.Context()); local != nil && err != nil {
		err = local
	}
	return response, err
}

// MonitoringProbeHTTPClient instruments independent provider clients without
// mutating their shared pool, DNS/TLS/proxy policy, or ordinary admin callers.
// HTTPUpstream already meters its transport and must not be wrapped again.
func MonitoringProbeHTTPClient(ctx context.Context, client *http.Client) *http.Client {
	if !monitoringProbeActive(ctx) || client == nil {
		return client
	}
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = monitoringProbeTransport{base: base}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

// Apply only to internally owned probes. Business requests never opt into this
// context through headers, JSON parameters, or downstream credentials.
func (s *MonitoringProbeBudgetService) Context(ctx context.Context, site string, recovery bool) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, monitoringProbeBudgetKey{}, monitoringProbeBudgetContext{
		service: s, sites: []string{site}, recovery: recovery, local: &monitoringProbeBudgetError{},
	})
}

func (s *MonitoringProbeBudgetService) qualityContext(ctx context.Context) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(WithHTTPUpstreamRedirectsDisabled(ctx), monitoringProbeBudgetKey{}, monitoringProbeBudgetContext{
		service: s, accountTest: true, local: &monitoringProbeBudgetError{},
	})
}

// Fixed-provider adapters bind their actual destination, not an optional base
// for a different native protocol. Explicit non-account probes keep their scope.
func withAccountTestMonitoringSite(ctx context.Context, site string) context.Context {
	if probe, ok := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext); ok && probe.accountTest {
		probe.sites = []string{site}
		return context.WithValue(ctx, monitoringProbeBudgetKey{}, probe)
	}
	return ctx
}

func (s *AccountTestService) SetMonitoringBudget(budget *MonitoringProbeBudgetService) {
	s.monitoringBudget = budget
	if s.openaiGatewayService != nil {
		s.openaiGatewayService.monitoringBudget = budget
	}
}

// BeginMonitoringProbeRequest is called once per physical RoundTrip, after
// credential/capacity checks. The returned finish must run after body closure.
func BeginMonitoringProbeRequest(req *http.Request) (budgeted *http.Request, release func(), resultErr error) {
	if req == nil {
		return req, nil, nil
	}
	probe, ok := req.Context().Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
	if !ok {
		return req, nil, nil
	}
	keys := config.UpstreamCredentialFingerprints(req)
	if probe.ollamaSession {
		// Only the internal official settings reader may identify web-session
		// cookies. Never substitute its unrelated inference API key, or let a
		// cookie hide another unrecognized final authorization header.
		var cookies []string
		for name, values := range req.Header {
			if strings.EqualFold(name, "Cookie") {
				cookies = append(cookies, values...)
			}
		}
		cookie, err := normalizeOllamaCloudUsageCookie(strings.Join(cookies, "; "))
		if err != nil || req.Method != http.MethodGet || !isExactOllamaCloudSettingsURL(req.URL) {
			keys = append(keys, config.UnverifiedUpstreamCredential)
		} else {
			parts := strings.Split(cookie, "; ")
			slices.Sort(parts)
			keys = append(keys, monitoringProbeHash("ollama-web-session\x00"+strings.Join(parts, "; ")))
		}
	}
	ctx, finish, err := beginMonitoringProbeRequest(req.Context(), req.URL, keys, req.URL != nil && req.Method == http.MethodPost && monitoringProbeGenerationPath(req.URL.Path))
	if finish == nil {
		return req, nil, err
	}
	return req.WithContext(ctx), finish, err
}

// WS handshakes and response.create turns share the HTTP accounting rules.
// Turn callers supply the fingerprints frozen at the successful handshake,
// never credentials reloaded from a potentially edited account.
func beginMonitoringProbeWSRequest(ctx context.Context, target string, keys []string, generation bool) (context.Context, func(), error) {
	probe, ok := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
	if !ok {
		return ctx, nil, nil
	}
	u, err := url.Parse(target)
	if err == nil {
		switch u.Scheme {
		case "ws":
			u.Scheme = "http"
		case "wss":
			u.Scheme = "https"
		default:
			err = ErrMonitoringProbeBudgetUnavailable
		}
	}
	if err != nil {
		probe.local.record(ErrMonitoringProbeBudgetUnavailable)
		return ctx, nil, ErrMonitoringProbeBudgetUnavailable
	}
	budgetCtx, finish, err := beginMonitoringProbeRequest(ctx, u, keys, generation)
	if usage, _ := budgetCtx.Value(monitoringProbeTokenKey{}).(*monitoringProbeTokenCollector); usage != nil {
		usage.setStream(true)
	}
	return budgetCtx, finish, err
}

func (l *openAIWSConnLease) beginMonitoringProbeRequest(ctx context.Context) (context.Context, func(), error) {
	target := ""
	var keys []string
	if l != nil && l.conn != nil {
		keys = l.conn.upstreamKeyFingerprints
		if l.conn.billingHandshake != nil && l.conn.billingHandshake.redirectsDisabled {
			target = l.conn.billingHandshake.target
		}
	}
	return beginMonitoringProbeWSRequest(ctx, target, keys, true)
}

// Releasing a handshake or completed turn cancels only its budget child, not
// the pooled socket or the parent that will send the next turn.
func finishMonitoringProbeWSRequest(ctx context.Context, finish func()) error {
	if finish == nil {
		return nil
	}
	finish()
	if errors.Is(context.Cause(ctx), ErrMonitoringProbeBudgetLeaseLost) {
		probe, _ := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
		probe.local.record(ErrMonitoringProbeBudgetLeaseLost)
		return ErrMonitoringProbeBudgetLeaseLost
	}
	return monitoringProbeLocalError(ctx)
}

func beginMonitoringProbeRequest(parent context.Context, target *url.URL, keys []string, meterTokens bool) (budgetCtx context.Context, release func(), resultErr error) {
	probe, ok := parent.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
	if !ok {
		return parent, nil, nil
	}
	defer func() { probe.local.record(resultErr) }()
	if probe.service == nil || probe.service.repo == nil {
		return parent, nil, ErrMonitoringProbeBudgetUnavailable
	}
	if local := monitoringProbeLocalError(parent); local != nil {
		return parent, nil, local
	}
	if err := context.Cause(parent); err != nil {
		return parent, nil, err
	}
	var site string
	for _, siteURL := range probe.sites {
		// Native OAuth/default-provider routes have no configured base URL.
		// Explicit protocol bases must never fall back to a different origin.
		if siteURL == "" && probe.accountTest && len(probe.sites) == 1 && target != nil {
			siteURL = target.Scheme + "://" + target.Host
		}
		candidate, err := CanonicalNewAPISite(siteURL)
		if err == nil && monitoringProbeSiteContains(candidate, target) && len(candidate) > len(site) {
			// Prefer the most specific configured tenant when bases overlap.
			site = candidate
		}
	}
	if site == "" {
		return parent, nil, ErrMonitoringProbeBudgetUnavailable
	}
	if len(keys) == 0 || slices.Contains(keys, config.UnverifiedUpstreamCredential) {
		return parent, nil, ErrMonitoringProbeBudgetUnavailable
	}
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	started := time.Now()
	acquireCtx, cancelAcquire := context.WithTimeout(parent, 2*time.Second)
	id, err := probe.service.repo.Acquire(acquireCtx, MonitoringProbeBudgetRequest{
		SiteHash: monitoringProbeHash(site), KeyHashes: keys, Recovery: probe.recovery, MeterTokens: meterTokens,
	})
	cancelAcquire()
	if err != nil {
		var denial *MonitoringProbeBudgetDenial
		if errors.As(err, &denial) {
			return parent, nil, denial
		}
		return parent, nil, ErrMonitoringProbeBudgetUnavailable
	}
	if id == "" {
		return parent, nil, ErrMonitoringProbeBudgetUnavailable
	}
	ctx, cancel := context.WithCancelCause(parent)
	var usage *monitoringProbeTokenCollector
	if meterTokens {
		usage = &monitoringProbeTokenCollector{}
		path := strings.TrimRight(target.Path, "/")
		if strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits") {
			usage.imageJSON = &monitoringProbeImageJSONFilter{}
		}
	}
	// A nested handshake must not inherit the parent's generation meter.
	ctx = context.WithValue(ctx, monitoringProbeTokenKey{}, usage)
	stop := keepRenewedLease(ctx, started, MonitoringProbeBudgetLeaseTTL,
		func(ctx context.Context) (bool, error) { return probe.service.repo.Renew(ctx, id) },
		func() { cancel(ErrMonitoringProbeBudgetLeaseLost) })
	finish := sync.OnceFunc(func() {
		stop()
		cancel(nil)
		ctx, cancelRelease := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelRelease()
		if err := probe.service.repo.Release(ctx, id, usage.snapshot()); err != nil {
			probe.local.record(ErrMonitoringProbeBudgetUnavailable)
		}
	})
	return ctx, finish, nil
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
	if usage, _ := b.ctx.Value(monitoringProbeTokenKey{}).(*monitoringProbeTokenCollector); usage != nil {
		usage.feed(p[:n], err == io.EOF)
	}
	if errors.Is(context.Cause(b.ctx), ErrMonitoringProbeBudgetLeaseLost) {
		probe, _ := b.ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
		probe.local.record(ErrMonitoringProbeBudgetLeaseLost)
		return n, ErrMonitoringProbeBudgetLeaseLost
	}
	return n, err
}

func (b *monitoringProbeBudgetBody) Close() error {
	err := b.ReadCloser.Close()
	if errors.Is(context.Cause(b.ctx), ErrMonitoringProbeBudgetLeaseLost) {
		probe, _ := b.ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
		probe.local.record(ErrMonitoringProbeBudgetLeaseLost)
	}
	b.finish()
	if local := monitoringProbeLocalError(b.ctx); errors.Is(local, ErrMonitoringProbeBudgetUnavailable) {
		return local
	}
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
			probe, _ := ctx.Value(monitoringProbeBudgetKey{}).(monitoringProbeBudgetContext)
			probe.local.record(ErrMonitoringProbeBudgetLeaseLost)
			return ErrMonitoringProbeBudgetLeaseLost
		}
		return err
	}
	response.Body = &monitoringProbeBudgetBody{ReadCloser: response.Body, ctx: ctx, finish: finish}
	if usage, _ := ctx.Value(monitoringProbeTokenKey{}).(*monitoringProbeTokenCollector); usage != nil {
		usage.setStream(strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream"))
	}
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

// IsMonitoringProbeBudgetFailure distinguishes local scheduling from provider
// failures in repository routing adapters. Error text is never evidence.
func IsMonitoringProbeBudgetFailure(err error) bool {
	return monitoringProbeDeferral(err, time.Now()) != nil
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
