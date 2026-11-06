//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestExcelBPSFinalSendRechecksAdmission(t *testing.T) {
	for _, path := range []string{"responses", "correction", "attachment"} {
		for _, state := range []string{"eligible", "rate_changed", "route_changed", "group_removed", "disabled", "read_failed", "no_gate"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				selected := excelAccount()
				selected.GroupIDs = []int64{9}
				selected.Extra["openai_excel_bps_mihomo"] = true
				rate := 0.2
				selected.RateMultiplier = &rate
				latest := *selected
				latest.Extra = maps.Clone(selected.Extra)
				repo := &turnAdmissionRepo{account: &latest}
				switch state {
				case "rate_changed":
					high := 0.9
					latest.RateMultiplier = &high
				case "route_changed":
					latest.Extra["openai_excel_bps"] = false
				case "group_removed":
					latest.GroupIDs = nil
				case "disabled":
					latest.Status = "disabled"
				case "read_failed":
					repo.err = errors.New("fixture unavailable")
				}
				ctx := t.Context()
				if state != "no_gate" {
					ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformOpenAI, threshold: 0.5})
				}
				calls := 0
				upstream := &bpsTestUpstream{send: func(_ *http.Request, _ string) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-fixture"}`))}, nil
				}}
				svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, requireLatestTurnAdmission: true}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				lease := &bpsTestLease{}
				acquire := func(context.Context, string, ...string) (string, excelBPSLease, error) {
					return "http://127.0.0.1:19000", lease, nil
				}
				var err error
				switch path {
				case "responses":
					var response *http.Response
					var returned excelBPSLease
					response, returned, _, err = svc.doExcelBPSRequest(ctx, c, selected, "fixture", []byte(`{"model":"gpt-5.6-sol"}`), "token", "account", acquire)
					if response != nil {
						require.NoError(t, response.Body.Close())
					}
					if returned != nil {
						returned.Release()
					}
				case "correction":
					request, requestErr := newExcelBPSRequest(ctx, []byte(`{"model":"gpt-5.6-sol"}`), "token", "account")
					require.NoError(t, requestErr)
					response, sendErr := svc.doPriorityObservedHTTP(request, "", selected)
					err = sendErr
					if response != nil {
						require.NoError(t, response.Body.Close())
					}
				case "attachment":
					_, err = svc.uploadExcelBPSAttachment(ctx, selected, "token", "account", "", basispoints.InlineAttachment{MIME: "image/png"})
				}
				allowed := state == "eligible" || state == "no_gate"
				if allowed {
					require.NoError(t, err)
					require.Equal(t, 1, calls)
				} else {
					require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
					require.Zero(t, calls)
				}
				if path == "responses" {
					require.Equal(t, 1, lease.releases)
					require.Zero(t, lease.failures, "a local veto is not proxy failure")
					_, recorded := c.Get(OpsUpstreamErrorsKey)
					require.False(t, recorded, "a local veto is not an upstream transport event")
				}
				if state == "no_gate" {
					require.Zero(t, repo.reads)
				} else {
					require.Equal(t, 1, repo.reads)
				}
			})
		}
	}
}

func TestExcelBPSRetryRechecksAfterUnsentAttempt(t *testing.T) {
	selected := excelAccount()
	selected.GroupIDs = []int64{9}
	selected.Extra["openai_excel_bps_mihomo"] = true
	low, high := 0.2, 0.9
	selected.RateMultiplier = &low
	latest := *selected
	latest.RateMultiplier = &high
	repo := &turnAdmissionRepo{accounts: []*Account{selected, &latest}}
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformOpenAI, threshold: 0.5})
	leases := []*bpsTestLease{{}, {}}
	acquired, calls := 0, 0
	acquire := func(context.Context, string, ...string) (string, excelBPSLease, error) {
		lease := leases[acquired]
		acquired++
		return fmt.Sprintf("http://127.0.0.1:%d", 19000+acquired), lease, nil
	}
	upstream := &bpsTestUpstream{send: func(r *http.Request, _ string) (*http.Response, error) {
		calls++
		httptrace.ContextClientTrace(r.Context()).GetConn("bps.openai.com:443")
		return nil, io.EOF
	}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, requireLatestTurnAdmission: true}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, _, _, err := svc.doExcelBPSRequest(ctx, c, selected, "fixture", []byte(`{"model":"gpt-5.6-sol"}`), "token", "account", acquire)
	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	require.Equal(t, 1, calls)
	require.Equal(t, 2, acquired)
	require.Equal(t, 2, repo.reads)
	require.Equal(t, 1, leases[0].failures)
	require.Zero(t, leases[1].failures)
	require.Equal(t, 1, leases[0].releases)
	require.Equal(t, 1, leases[1].releases)
}

func TestExcelBPSAvailabilityDoesNotReuseHTTPProbeScope(t *testing.T) {
	for _, path := range []string{"responses", "attachment"} {
		for _, configured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/configured=%t", path, configured), func(t *testing.T) {
				account := excelAccount()
				repo := &availabilityAdmissionRepo{}
				if configured {
					repo.reports = []*AvailabilityReport{availabilityAdmissionPass(account.ID, "gpt-5.6-sol")}
				}
				calls := 0
				svc := &OpenAIGatewayService{availabilityReader: repo, httpUpstream: &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-fixture"}`))}, nil
				}}}
				var err error
				if path == "responses" {
					request, requestErr := newExcelBPSRequest(t.Context(), []byte(`{"model":"gpt-5.6-sol","input":"fixture"}`), "token", "account")
					require.NoError(t, requestErr)
					resp, sendErr := svc.doPriorityObservedHTTP(request, "", account)
					err = sendErr
					if resp != nil {
						require.NoError(t, resp.Body.Close())
					}
				} else {
					_, err = svc.uploadExcelBPSAttachment(t.Context(), account, "token", "account", "", basispoints.InlineAttachment{MIME: "image/png"})
				}
				if configured {
					require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
					require.Zero(t, calls)
				} else {
					require.NoError(t, err)
					require.Equal(t, 1, calls)
				}
				require.Equal(t, 1, repo.reads)
			})
		}
	}
}

func TestExcelBPSForwardRechecksAfterAttachmentOrRejection(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	for _, path := range []string{"attachment", "encrypted_retry"} {
		t.Run(path, func(t *testing.T) {
			selected := excelAccount()
			selected.GroupIDs = []int64{9}
			low, high := 0.2, 0.9
			selected.RateMultiplier = &low
			latest := *selected
			latest.RateMultiplier = &high
			repo := &turnAdmissionRepo{account: selected}
			ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformOpenAI, threshold: 0.5})
			svc := openAIClientToolsTestService(nil)
			svc.accountRepo = repo
			svc.requireLatestTurnAdmission = true
			var body []byte
			if path == "attachment" {
				enableNativeAttachments(svc)
				body, _ = nativeGatewayBody(t)
			} else {
				body = excelBPSEncryptedHistoryRequest(false)
			}
			calls := 0
			var firstBody *excelBPSRepairBody
			svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
				calls++
				if calls == 1 {
					repo.account = &latest
					content := excelBPSInvalidCiphertext
					status := http.StatusBadRequest
					if path == "attachment" {
						require.Equal(t, basispoints.AttachmentsURL, req.URL.String())
						status, content = http.StatusOK, `{"openai_file_id":"file-fixture"}`
					}
					firstBody = &excelBPSRepairBody{Reader: strings.NewReader(content)}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: firstBody}, nil
				}
				if path == "attachment" && req.URL.String() == basispoints.AttachmentsURL {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-fixture"}`))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
			}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
			_, err := svc.forwardExcelBPS(ctx, c, selected, body, time.Now())
			require.Error(t, err)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "admission_unavailable")
			require.Equal(t, 1, calls, "no second physical send after the rate changes")
			require.True(t, firstBody.closed.Load())
			require.True(t, selected.Schedulable)
			repo.account = selected
			rec = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
			_, err = svc.forwardExcelBPS(ctx, c, selected, body, time.Now())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code)
			want := 2
			if path == "attachment" {
				want = 3
			}
			require.Equal(t, want, calls, "the original route resumes after admission recovers")
		})
	}
}

func TestExcelBPSLocalDenialsDoNotCountAsSubmissionOrProxyFailure(t *testing.T) {
	for _, state := range []string{"profit", "auth", "controlled"} {
		t.Run(state, func(t *testing.T) {
			account := excelAccount()
			account.GroupIDs = []int64{9}
			account.Extra["openai_excel_bps_mihomo"] = true
			low, high := 0.2, 0.9
			account.RateMultiplier = &low
			latest := *account
			latest.RateMultiplier = &high
			ctx := t.Context()
			mode := &controlledExperimentMode{channel: "bps"}
			ctx = context.WithValue(ctx, controlledExperimentContextKey{}, mode)
			repo := &turnAdmissionRepo{account: &latest}
			if state == "profit" {
				ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformOpenAI, threshold: 0.5})
			}
			if state == "auth" {
				account.AuthConnectionID = "fixture"
			}
			if state == "controlled" {
				mode.channel = "native_http"
			}
			lease := &bpsTestLease{}
			calls := 0
			svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true, httpUpstream: &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) { calls++; return nil, io.EOF }}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, _, _, err := svc.doExcelBPSRequest(ctx, c, account, "fixture", []byte(`{"model":"gpt-5.6-sol"}`), "token", "account", func(context.Context, string, ...string) (string, excelBPSLease, error) {
				return "http://127.0.0.1:19000", lease, nil
			})
			require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
			require.Zero(t, calls)
			require.Zero(t, mode.submissions.Load())
			require.Equal(t, 1, lease.releases)
			require.Zero(t, lease.failures)
			_, recorded := c.Get(OpsUpstreamErrorsKey)
			require.False(t, recorded)
		})
	}
}

func TestExcelBPSLegacyCredentialsCannotOutliveManagedPolicy(t *testing.T) {
	for _, path := range []string{"responses", "attachment"} {
		t.Run(path, func(t *testing.T) {
			reauth, repo := connectionTypeService(AccountTypeOAuth)
			repo.managed = true
			account := excelAccount()
			calls := 0
			svc := &OpenAIGatewayService{excelOAuthReauth: reauth, httpUpstream: &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) { calls++; return nil, io.EOF }}}
			var err error
			if path == "attachment" {
				_, err = svc.uploadExcelBPSAttachment(t.Context(), account, "token", "account", "", basispoints.InlineAttachment{MIME: "image/png"})
			} else {
				request, requestErr := newExcelBPSRequest(t.Context(), []byte(`{"model":"gpt-5.6-sol"}`), "token", "account")
				require.NoError(t, requestErr)
				_, err = svc.doPriorityObservedHTTP(request, "", account)
			}
			require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
			require.Zero(t, calls)
		})
	}
}

func TestExcelBPSManagedParentRouteChangeIsRejected(t *testing.T) {
	for _, parentType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, state := range []string{"unchanged", "proxy", "proxy_pool", "model_mapping", "missing_origin"} {
			t.Run(parentType+"/"+state, func(t *testing.T) {
				parent := excelAccount()
				parent.Type = parentType
				parent.GroupIDs = []int64{9}
				parent.Extra["openai_connections_managed"] = true
				parent.Extra["openai_excel_bps"] = false
				parent.Extra["openai_excel_bps_mihomo"] = true
				rate := 0.2
				parent.RateMultiplier = &rate
				selected := *parent
				selected.Type = AccountTypeOAuth
				selected.Extra = maps.Clone(parent.Extra)
				selected.Extra["openai_excel_bps"] = true
				selected.AuthConnectionID = "fixture-connection"
				origin := excelBPSParentRouteFingerprint(parent)
				selected.excelBPSParentRoute = &origin
				latest := *parent
				latest.Extra = maps.Clone(parent.Extra)
				latest.Credentials = maps.Clone(parent.Credentials)
				switch state {
				case "proxy":
					latest.Proxy = &Proxy{Protocol: "http", Host: "new-proxy.invalid", Port: 8080}
				case "proxy_pool":
					latest.Extra[ExcelBPSProxySourceKey] = ExcelBPSProxySourceIPPool
				case "model_mapping":
					latest.Credentials["model_mapping"] = map[string]any{"gpt-5.6-sol": "different-model"}
				case "missing_origin":
					selected.excelBPSParentRoute = nil
				}
				ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformOpenAI, threshold: 0.5})
				repo := &turnAdmissionRepo{account: &latest}
				svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true}
				request, err := newExcelBPSRequest(ctx, []byte(`{"model":"gpt-5.6-sol"}`), "token", "account")
				require.NoError(t, err)
				_, err = svc.guardOpenAIProfitRequest(request, &selected)
				if state == "unchanged" {
					require.NoError(t, err)
				} else {
					require.True(t, IsOpenAITurnAdmissionError(err), "a live connection does not authorize a changed parent route: %v", err)
					if state != "missing_origin" {
						repo.account = parent
						_, err = svc.guardOpenAIProfitRequest(request, &selected)
						require.NoError(t, err, "the original parent route must recover")
					}
				}
			})
		}
	}
}

func TestExcelBPSManagedProjectionCapturesParentRoute(t *testing.T) {
	reauth, connections := connectionTypeService(AccountTypeOAuth)
	connections.managed = true
	connection := &OpenAIAuthConnection{ID: "c910c45b-79a0-4367-a1dc-60e12210e199", AccountID: 42, Kind: "bps", Status: "active", RefreshState: "idle", Revision: 1, Preferred: true, Capabilities: []string{"responses"}}
	require.NoError(t, reauth.encryptConnectionCredentials(connection, map[string]any{
		"access_token": "fixture-token", "chatgpt_account_id": "fixture-account", "expires_at": fmt.Sprint(time.Now().Add(time.Hour).Unix()),
	}))
	connections.rows = []*OpenAIAuthConnection{connection}
	parent, err := reauth.accounts.GetAccount(t.Context(), 42)
	require.NoError(t, err)
	parent.Status, parent.Schedulable, parent.GroupIDs = StatusActive, true, []int64{9}
	parent.Extra = map[string]any{"openai_excel_bps_mihomo": true}
	parent.Proxy = &Proxy{Protocol: "http", Host: "original-proxy.invalid", Port: 8080}
	rate := 0.2
	parent.RateMultiplier = &rate
	svc := &OpenAIGatewayService{excelOAuthReauth: reauth, accountRepo: &turnAdmissionRepo{account: parent}, requireLatestTurnAdmission: true}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
	selected, err := svc.prepareAuthConnection(t.Context(), c, parent, nil, "gpt-5.6-sol")
	require.NoError(t, err)
	require.NotNil(t, selected.excelBPSParentRoute)
	require.Equal(t, excelBPSParentRouteFingerprint(parent), *selected.excelBPSParentRoute)
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformOpenAI, threshold: 0.5})
	request, err := newExcelBPSRequest(ctx, []byte(`{"model":"gpt-5.6-sol"}`), "fixture-token", "fixture-account")
	require.NoError(t, err)
	_, err = svc.guardOpenAIProfitRequest(request, selected)
	require.NoError(t, err)

	calls := 0
	svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		content := "ok"
		if req.URL.String() == basispoints.AttachmentsURL {
			content = `{"openai_file_id":"file-fixture"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(content))}, nil
	}}
	for _, path := range []string{"responses", "attachment"} {
		t.Run(path, func(t *testing.T) {
			send := func() error {
				if path == "attachment" {
					_, sendErr := svc.uploadExcelBPSAttachment(ctx, selected, "fixture-token", "fixture-account", "", basispoints.InlineAttachment{MIME: "image/png"})
					return sendErr
				}
				req, requestErr := newExcelBPSRequest(ctx, []byte(`{"model":"gpt-5.6-sol"}`), "fixture-token", "fixture-account")
				if requestErr != nil {
					return requestErr
				}
				resp, sendErr := svc.doPriorityObservedHTTP(req, "", selected)
				if resp != nil {
					require.NoError(t, resp.Body.Close())
				}
				return sendErr
			}
			before := calls
			require.NoError(t, send())
			require.Equal(t, before+1, calls)
			connections.rows[0].Revision++
			err := send()
			require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
			require.Equal(t, before+1, calls, "a revoked connection must not send")
			connections.rows[0].Revision--
			require.NoError(t, send())
			require.Equal(t, before+2, calls, "restoring the bound connection must resume sending")
		})
	}
}

func TestExcelBPSToolRepairRechecksAfterOutputAndPreservesUsage(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	selected := excelAccount()
	selected.GroupIDs = []int64{9}
	low, high := 0.2, 0.9
	selected.RateMultiplier = &low
	latest := *selected
	latest.RateMultiplier = &high
	repo := &turnAdmissionRepo{account: selected}
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: 9, platform: PlatformOpenAI, threshold: 0.5})
	svc := openAIClientToolsTestService(nil)
	svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
	calls := 0
	first := &excelBPSRepairBody{Reader: strings.NewReader(excelBPSRepairWire(t, "fixture", "Run client tool", "text(42);"))}
	svc.httpUpstream = &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) {
		calls++
		repo.account = &latest
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: first}, nil
	}}
	body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"test correction","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	result, err := svc.forwardExcelBPS(ctx, c, selected, body, time.Now())
	require.Error(t, err)
	require.Equal(t, 1, calls)
	require.True(t, first.closed.Load())
	require.NotNil(t, result)
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), "basispoints_protocol_error")
}

func TestExcelBPSImagesAdmissionFailureIsNotTransportOrFallback(t *testing.T) {
	account := excelBPSImagesAccount()
	repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(account.ID, "gpt-image-2")}}
	calls := 0
	svc := newOpenAIImagesTestService(&bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) { calls++; return openAIImagesJSONResponse(), nil }})
	svc.availabilityReader = repo
	body := []byte(`{"model":"gpt-image-2","prompt":"fixture"}`)
	c, rec := newOpenAIImagesTestContext(t, body)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	_, fallback, err := svc.forwardExcelBPSImages(t.Context(), c, account, parsed, "gpt-image-2", "gpt-image-2", time.Now())
	require.Error(t, err)
	require.False(t, fallback)
	require.Zero(t, calls)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "admission_unavailable")
	_, recorded := c.Get(OpsUpstreamErrorsKey)
	require.False(t, recorded)
}
