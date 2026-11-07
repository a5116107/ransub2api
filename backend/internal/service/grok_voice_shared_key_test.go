//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type grokVoiceKeyAuthority struct {
	HTTPUpstream
	err            error
	nilContext     bool
	releaseOnError bool
	fingerprints   []string
	takes          atomic.Int32
	releases       atomic.Int32
	cancel         context.CancelCauseFunc
	onAcquire      func()
}

func (a *grokVoiceKeyAuthority) AcquireUpstreamKeySlots(ctx context.Context, fingerprints []string) (context.Context, func(), error) {
	a.takes.Add(1)
	a.fingerprints = append([]string(nil), fingerprints...)
	if a.onAcquire != nil {
		a.onAcquire()
	}
	if a.err != nil {
		if a.releaseOnError {
			return ctx, func() { a.releases.Add(1) }, a.err
		}
		return ctx, nil, a.err
	}
	if a.nilContext {
		return nil, func() { a.releases.Add(1) }, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	a.cancel = cancel
	return ctx, sync.OnceFunc(func() { a.releases.Add(1); cancel(context.Canceled) }), nil
}

func TestGrokVoiceAvailabilityAdmissionBeforeDial(t *testing.T) {
	for _, endpoint := range []string{"realtime", "tts"} {
		for _, scenario := range []string{"no_reader", "no_rules", "configured_pass", "configured_fail", "paused", "read_error", "probe", "enabled_during_key_acquire"} {
			t.Run(endpoint+"/"+scenario, func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
				account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
				report := availabilityAdmissionPass(account.ID, "fixture")
				repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
				authority := &grokVoiceKeyAuthority{}
				dialer := &grokVoiceKeyDialer{}
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, availabilityReader: repo, openaiWSPassthroughDialer: dialer}
				ctx := t.Context()
				allowed := false
				switch scenario {
				case "no_reader":
					svc.availabilityReader, allowed = nil, true
				case "no_rules":
					repo.reports, allowed = nil, true
				case "configured_fail":
					report.Models[0].Status = "fail"
				case "paused":
					report.Enabled = false
				case "read_error":
					repo.readErr = errors.New("fixture availability outage")
				case "probe":
					ctx, allowed = context.WithValue(ctx, qualityProbeContextKey{}, true), true
				case "enabled_during_key_acquire":
					repo.reports = nil
					authority.onAcquire = func() { repo.reports = []*AvailabilityReport{report} }
				}
				var upstream *GrokRealtimeUpstream
				var err error
				if endpoint == "realtime" {
					upstream, err = svc.OpenGrokRealtime(ctx, account, "shared-fixture", "fixture")
				} else {
					upstream, err = svc.OpenGrokTTSStream(ctx, account, "shared-fixture", url.Values{})
				}
				if allowed {
					require.NoError(t, err)
					require.NotNil(t, upstream)
					require.EqualValues(t, 1, dialer.sends.Load())
					require.NoError(t, upstream.Close())
				} else {
					require.Nil(t, upstream)
					require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
					require.Zero(t, dialer.sends.Load(), "unobserved voice scope must not dial")
				}
				require.EqualValues(t, 1, authority.releases.Load(), "admission rejection must release the acquired key")
			})
		}
	}
}

func TestGrokVoiceAvailabilityAdmissionWithoutSharedKeyPolicy(t *testing.T) {
	dialer := &grokVoiceKeyDialer{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, openaiWSPassthroughDialer: dialer,
		availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(1, "fixture")}}}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	upstream, err := svc.OpenGrokRealtime(t.Context(), account, "fixture", "fixture")
	require.Nil(t, upstream)
	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	require.Zero(t, dialer.sends.Load())
}

func TestGrokVoiceKeyLeaseLossDuringAvailabilityCheck(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	authority := &grokVoiceKeyAuthority{}
	repo := &availabilityAdmissionRepo{onRead: func(context.Context, *Account) { authority.cancel(ErrUpstreamKeyLease) }}
	dialer := &grokVoiceKeyDialer{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, availabilityReader: repo, openaiWSPassthroughDialer: dialer}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	upstream, err := svc.OpenGrokRealtime(t.Context(), account, "fixture", "fixture")
	require.Nil(t, upstream)
	require.ErrorIs(t, err, ErrUpstreamKeyLease)
	require.Zero(t, dialer.sends.Load())
	require.EqualValues(t, 1, authority.releases.Load())
}

type grokVoiceKeyDialer struct {
	sends             atomic.Int32
	conn              openAIWSClientConn
	err               error
	redirectsDisabled bool
	onDial            func()
	nilConn           bool
	connOnError       bool
}

func (d *grokVoiceKeyDialer) Dial(ctx context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	d.sends.Add(1)
	d.redirectsDisabled = HTTPUpstreamRedirectsDisabled(ctx)
	if d.onDial != nil {
		d.onDial()
	}
	if d.err != nil {
		if d.connOnError {
			return d.conn, 0, nil, d.err
		}
		return nil, 0, nil, d.err
	}
	if d.nilConn {
		return nil, http.StatusSwitchingProtocols, nil, nil
	}
	if d.conn != nil {
		return d.conn, http.StatusSwitchingProtocols, nil, nil
	}
	return &openAIWSFakeConn{}, http.StatusSwitchingProtocols, nil, nil
}

type grokVoiceKeyConn struct {
	openAIWSFakeConn
	closed atomic.Int32
	forced atomic.Int32
}

func (c *grokVoiceKeyConn) Close() error    { c.closed.Add(1); return nil }
func (c *grokVoiceKeyConn) CloseNow() error { c.forced.Add(1); return nil }

type grokVoiceBlockingCloseConn struct {
	grokVoiceKeyConn
	started chan struct{}
	finish  chan struct{}
}

func (c *grokVoiceBlockingCloseConn) CloseNow() error {
	close(c.started)
	<-c.finish
	return c.grokVoiceKeyConn.CloseNow()
}

func TestGrokVoiceSharedKeyRejectsBeforeWebSocketDial(t *testing.T) {
	for _, endpoint := range []string{"realtime", "tts"} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("shared-fixture"): 1}}
			authority := &grokVoiceKeyAuthority{err: ErrUpstreamKeyCapacity}
			dialer := &grokVoiceKeyDialer{}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, openaiWSPassthroughDialer: dialer}
			account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
			var err error
			if endpoint == "realtime" {
				_, err = svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
			} else {
				_, err = svc.OpenGrokTTSStream(t.Context(), account, "shared-fixture", url.Values{})
			}
			require.True(t, IsUpstreamKeyAdmissionFailure(err))
			require.Equal(t, []string{config.UpstreamKeyFingerprint("shared-fixture")}, authority.fingerprints)
			require.Zero(t, dialer.sends.Load())
		})
	}
}

func TestGrokVoiceSharedKeySessionLifetime(t *testing.T) {
	for _, endpoint := range []string{"realtime", "tts"} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
			authority := &grokVoiceKeyAuthority{}
			conn := &grokVoiceKeyConn{}
			dialer := &grokVoiceKeyDialer{conn: conn}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, openaiWSPassthroughDialer: dialer}
			account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
			var upstream *GrokRealtimeUpstream
			var err error
			if endpoint == "realtime" {
				upstream, err = svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
			} else {
				upstream, err = svc.OpenGrokTTSStream(t.Context(), account, "shared-fixture", url.Values{})
			}
			require.NoError(t, err)
			require.NotNil(t, upstream)
			require.EqualValues(t, 1, dialer.sends.Load())
			require.True(t, dialer.redirectsDisabled)
			require.Zero(t, authority.releases.Load(), "handshake completion must not release an active session")
			require.NoError(t, upstream.Close())
			require.NoError(t, upstream.Close())
			require.EqualValues(t, 1, authority.releases.Load())
			require.Zero(t, conn.closed.Load())
			require.EqualValues(t, 1, conn.forced.Load())
		})
	}
}

func TestGrokVoiceWithoutSharedPolicyPreservesHandshake(t *testing.T) {
	conn := &grokVoiceKeyConn{}
	dialer := &grokVoiceKeyDialer{conn: conn}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, openaiWSPassthroughDialer: dialer}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	ctx, cancel := context.WithCancel(t.Context())
	upstream, err := svc.OpenGrokRealtime(ctx, account, "shared-fixture", "fixture")
	require.NoError(t, err)
	require.False(t, dialer.redirectsDisabled)
	cancel()
	require.NoError(t, upstream.Close())
	require.EqualValues(t, 1, conn.closed.Load())
	require.Zero(t, conn.forced.Load())
}

func TestGrokVoiceSharedKeyLeaseLossClosesSession(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	authority := &grokVoiceKeyAuthority{}
	conn := &grokVoiceKeyConn{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, openaiWSPassthroughDialer: &grokVoiceKeyDialer{conn: conn}}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	upstream, err := svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
	require.NoError(t, err)
	authority.cancel(ErrUpstreamKeyLease)
	require.Eventually(t, func() bool { return conn.forced.Load() == 1 && authority.releases.Load() == 1 }, time.Second, time.Millisecond)
	require.NoError(t, upstream.Close())
	require.EqualValues(t, 1, authority.releases.Load())
	require.Zero(t, conn.closed.Load())
}

func TestGrokVoiceSharedKeyLeaseLossDuringClose(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	authority := &grokVoiceKeyAuthority{}
	conn := &grokVoiceBlockingCloseConn{started: make(chan struct{}), finish: make(chan struct{})}
	var finishOnce sync.Once
	t.Cleanup(func() { finishOnce.Do(func() { close(conn.finish) }) })
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, openaiWSPassthroughDialer: &grokVoiceKeyDialer{conn: conn}}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	upstream, err := svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- upstream.Close() }()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("upstream close did not begin")
	}
	authority.cancel(ErrUpstreamKeyLease)
	require.Zero(t, authority.releases.Load(), "the slot remains held until upstream close returns")
	finishOnce.Do(func() { close(conn.finish) })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("upstream close did not finish")
	}
	require.NoError(t, upstream.Close())
	require.EqualValues(t, 1, conn.forced.Load())
	require.EqualValues(t, 1, authority.releases.Load())
}

func TestGrokVoiceSharedKeyRequiresAuthorityAndReleasesFailedDial(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	dialer := &grokVoiceKeyDialer{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, openaiWSPassthroughDialer: dialer}
	_, err := svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
	require.True(t, IsUpstreamKeyAdmissionFailure(err))
	require.Zero(t, dialer.sends.Load())
	authority := &grokVoiceKeyAuthority{}
	dialer.err = context.DeadlineExceeded
	svc.httpUpstream = authority
	_, err = svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 1, authority.releases.Load())
}

func TestGrokVoiceSharedKeyCleansUpInvalidAuthorityAndDialerResults(t *testing.T) {
	for _, tc := range []struct {
		name           string
		authority      *grokVoiceKeyAuthority
		dialer         *grokVoiceKeyDialer
		wantAdmission  bool
		wantDial       int32
		wantConnClosed bool
	}{
		{"authority_error_with_release", &grokVoiceKeyAuthority{err: ErrUpstreamKeyPolicy, releaseOnError: true}, &grokVoiceKeyDialer{}, true, 0, false},
		{"authority_nil_context", &grokVoiceKeyAuthority{nilContext: true}, &grokVoiceKeyDialer{}, true, 0, false},
		{"dial_nil_connection", &grokVoiceKeyAuthority{}, &grokVoiceKeyDialer{nilConn: true}, false, 1, false},
		{"dial_connection_with_error", &grokVoiceKeyAuthority{}, &grokVoiceKeyDialer{conn: &grokVoiceKeyConn{}, err: context.DeadlineExceeded, connOnError: true}, false, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: tc.authority, openaiWSPassthroughDialer: tc.dialer}
			account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
			upstream, err := svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
			require.Nil(t, upstream)
			require.Error(t, err)
			require.Equal(t, tc.wantAdmission, IsUpstreamKeyAdmissionFailure(err))
			require.Equal(t, tc.wantDial, tc.dialer.sends.Load())
			require.EqualValues(t, 1, tc.authority.releases.Load())
			if tc.wantConnClosed {
				conn, ok := tc.dialer.conn.(*grokVoiceKeyConn)
				require.True(t, ok)
				require.EqualValues(t, 1, conn.forced.Load())
			}
		})
	}
}

func TestGrokVoiceSharedKeyLeaseLossDuringDial(t *testing.T) {
	for _, dialFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "connected", true: "dial_failed"}[dialFails], func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
			authority := &grokVoiceKeyAuthority{}
			conn := &grokVoiceKeyConn{}
			dialer := &grokVoiceKeyDialer{conn: conn, onDial: func() { authority.cancel(ErrUpstreamKeyLease) }}
			if dialFails {
				dialer.err = context.Canceled
			}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, openaiWSPassthroughDialer: dialer}
			account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
			upstream, err := svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
			require.Nil(t, upstream)
			require.ErrorIs(t, err, ErrUpstreamKeyLease)
			require.EqualValues(t, 1, authority.releases.Load())
			if !dialFails {
				require.EqualValues(t, 1, conn.forced.Load())
			}
		})
	}
}

func TestProbeGrokRealtimeUsesSharedKeyAdmission(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	authority := &grokVoiceKeyAuthority{err: ErrUpstreamKeyCapacity}
	dialer := &grokVoiceKeyDialer{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: authority, openaiWSPassthroughDialer: dialer}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	err := svc.ProbeGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
	require.True(t, IsUpstreamKeyAdmissionFailure(err))
	require.Zero(t, dialer.sends.Load())
	authority.err = nil
	require.NoError(t, svc.ProbeGrokRealtime(t.Context(), account, "shared-fixture", "fixture"))
	require.EqualValues(t, 1, dialer.sends.Load())
	require.EqualValues(t, 1, authority.releases.Load())
}

func TestAccountGrokRealtimeProbeUsesSharedKeyAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	authority := &grokVoiceKeyAuthority{err: ErrUpstreamKeyCapacity}
	dialer := &grokVoiceKeyDialer{}
	svc := &AccountTestService{cfg: cfg, httpUpstream: authority, grokWSDialer: dialer}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil)
	err := svc.testGrokRealtime(c, t.Context(), account, "shared-fixture", "fixture")
	require.Error(t, err)
	require.Zero(t, dialer.sends.Load(), "a full shared credential must not open an administrator probe")
	require.EqualValues(t, 1, authority.takes.Load())
	require.Contains(t, recorder.Body.String(), "shared")
}

func TestAccountGrokRealtimeProbeLeaseLossDoesNotReportSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	authority := &grokVoiceKeyAuthority{}
	dialer := &grokVoiceKeyDialer{conn: &grokVoiceKeyConn{}}
	svc := &AccountTestService{cfg: cfg, httpUpstream: authority, grokWSDialer: dialer}
	account := &Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}
	dialer.onDial = func() { authority.cancel(ErrUpstreamKeyLease) }
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil)
	err := svc.testGrokRealtime(c, t.Context(), account, "shared-fixture", "fixture")
	require.Error(t, err)
	require.NotContains(t, recorder.Body.String(), `"success":true`)
	require.EqualValues(t, 1, authority.releases.Load())
	require.True(t, strings.Contains(recorder.Body.String(), "shared"))
}
