//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func regionalAvailabilityDoc(t *testing.T, account *Account, reports []*AvailabilityReport) []byte {
	t.Helper()
	if reports == nil {
		reports = []*AvailabilityReport{}
	}
	doc := RegionalSchedulerDocument{Version: 2, Buckets: []RegionalSchedulerBucket{{
		Bucket: SchedulerBucket{Platform: PlatformOpenAI, Mode: SchedulerModeSingle}, Accounts: []*Account{account},
	}}, Availability: []RegionalAccountAvailability{{AccountID: account.ID, Reports: reports}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	return raw
}

func TestRegionalAvailabilityAdmissionNeverReadsPrimary(t *testing.T) {
	for _, candidateOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy_packet", true: "candidate_only"}[candidateOnly], func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
			scheduler := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
			if candidateOnly {
				scheduler.SetLocalSnapshotReader(NewLocalSchedulerSnapshotStore(nil))
			} else {
				replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
				require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, regionalDoc(t, account)))
				reader, err := NewRegionalSchedulerReader(replica)
				require.NoError(t, err)
				scheduler.SetLocalSnapshotReader(reader)
			}
			primary := &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(account.ID, "target")}}
			svc := &OpenAIGatewayService{schedulerSnapshot: scheduler, availabilityReader: primary}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"target"}`))
			require.NoError(t, err)
			err = svc.admitAvailabilityRequest(req.Context(), req, account)
			require.Zero(t, primary.reads, "regional availability must never fall back to the primary repository")
			require.True(t, IsOpenAITurnAdmissionError(err), "an absent local policy must deny admission")
		})
	}
}

func TestRegionalAvailabilityHTTPStopRecoveryAndScope(t *testing.T) {
	var sends atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	t.Cleanup(server.Close)
	_, svc, account := availabilityWSTestServices(t, server)
	account.ID = 1
	replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
	reader, err := NewRegionalSchedulerReader(replica)
	require.NoError(t, err)
	svc.schedulerSnapshot = NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
	svc.schedulerSnapshot.SetLocalSnapshotReader(reader)
	primary := &availabilityAdmissionRepo{readErr: errors.New("primary must stay offline")}
	svc.availabilityReader = primary
	for _, state := range []string{"pass", "fail", "paused", "unknown", "expired", "other_model", "other_tier", "other_protocol", "route_changed", "recovery", "no_rules", "nil_primary"} {
		t.Run(state, func(t *testing.T) {
			report := availabilityAdmissionPass(account.ID, "target")
			report.PlanID = 41
			reports := []*AvailabilityReport{report}
			selected := *account
			allowed := false
			switch state {
			case "pass", "recovery", "nil_primary":
				allowed = true
				if state == "nil_primary" {
					svc.availabilityReader = nil
				}
			case "no_rules":
				reports, allowed = nil, true
			case "fail":
				report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
			case "paused":
				report.Enabled = false
			case "unknown":
				report.Models[0].Status, report.Models[0].AvailabilityState = "unknown", AvailabilityState{}
			case "expired":
				// The packet is fresh, but its primary evaluation is old and
				// its observation has expired while waiting for delivery.
				report.EvaluatedAt = time.Now().Add(-2 * time.Minute)
				until := time.Now().Add(-time.Millisecond)
				report.Models[0].ValidUntil = &until
			case "other_model":
				report.Models[0].ModelID = "unobserved"
			case "other_tier":
				report.Models[0].Decision.Scope.ServiceTier = "priority"
			case "other_protocol":
				report.Models[0].Decision.Scope.Protocol = "responses_ws_v2"
			case "route_changed":
				selected.Credentials = map[string]any{"api_key": "old-key", "base_url": server.URL}
			}
			require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, regionalAvailabilityDoc(t, account, reports)))
			before := sends.Load()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"target","input":"fixture"}`))
			require.NoError(t, err)
			response, err := svc.doOpenAIUpstream(req, "", &selected)
			if allowed {
				require.NoError(t, err)
				require.NotNil(t, response)
				require.NoError(t, response.Body.Close())
				require.Equal(t, before+1, sends.Load())
			} else {
				require.Nil(t, response)
				require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
				require.Equal(t, before, sends.Load(), "a local denial must send zero HTTP requests")
			}
			require.Zero(t, primary.reads)
		})
	}
}

func TestRegionalAvailabilityRejectsIncompleteOrConflictingDocuments(t *testing.T) {
	for _, change := range []string{"missing", "foreign", "duplicate_account", "nil_reports", "nil_report", "wrong_account", "duplicate_plan", "missing_clock", "duplicate_model", "legacy_with_reports"} {
		t.Run(change, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI}
			report := availabilityAdmissionPass(account.ID, "target")
			report.PlanID = 41
			var doc RegionalSchedulerDocument
			require.NoError(t, json.Unmarshal(regionalAvailabilityDoc(t, account, []*AvailabilityReport{report}), &doc))
			entry := &doc.Availability[0]
			switch change {
			case "missing":
				doc.Availability = []RegionalAccountAvailability{}
			case "foreign":
				entry.AccountID = 2
			case "duplicate_account":
				doc.Availability = append(doc.Availability, *entry)
			case "nil_reports":
				entry.Reports = nil
			case "nil_report":
				entry.Reports = []*AvailabilityReport{nil}
			case "wrong_account":
				entry.Reports[0].AccountID = 2
			case "duplicate_plan":
				entry.Reports = append(entry.Reports, entry.Reports[0])
			case "missing_clock":
				entry.Reports[0].EvaluatedAt = time.Time{}
			case "duplicate_model":
				entry.Reports[0].Models = append(entry.Reports[0].Models, entry.Reports[0].Models[0])
			case "legacy_with_reports":
				doc.Version = 1
			}
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			require.Error(t, RegionalSchedulerValidator([]int64{1, 2})(raw))
		})
	}
}

func TestRegionalAvailabilityNativeWSRechecksLocalPolicyEachTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeDedicated, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			upstream := newStagedPassthroughConn()
			svc := newPassthroughLifecycleService(cfg, upstream)
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&stagedPassthroughDialer{conn: &availabilityWSStagedWriter{upstream}})
			t.Cleanup(pool.Close)
			svc.openaiWSPool = pool
			account := passthroughLifecycleAccount()
			account.ID = 1
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			report := availabilityAdmissionPass(account.ID, "gpt-5.1")
			report.PlanID = 41
			report.Models[0].Decision.Scope.Protocol = "responses_ws_v2"
			report.Models[0].Decision.Scope.Stream = true
			replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
			publish := func() {
				t.Helper()
				require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, regionalAvailabilityDoc(t, account, []*AvailabilityReport{report})))
			}
			publish()
			reader, err := NewRegionalSchedulerReader(replica)
			require.NoError(t, err)
			svc.schedulerSnapshot = NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
			svc.schedulerSnapshot.SetLocalSnapshotReader(reader)
			require.Nil(t, svc.availabilityReader, "local-only deployments must still install every physical-send check")
			ctx, cancel := context.WithCancel(t.Context())
			server, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
			t.Cleanup(func() { cancel(); server.Close() })
			payload := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":"hi"}`)
			client := dialPassthroughLifecycleClientWithPayload(t, server, string(payload))
			t.Cleanup(func() { _ = client.CloseNow() })
			select {
			case <-upstream.writes:
			case <-time.After(3 * time.Second):
				t.Fatal("passing local evidence did not admit the first turn")
			}
			upstream.Send(`{"type":"response.output_text.delta","delta":"21","response_id":"fixture"}`)
			upstream.Send(`{"type":"response.completed","response":{"id":"fixture","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
			for range 2 {
				_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
				require.NoError(t, err)
			}
			report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
			publish()
			require.NoError(t, client.Write(t.Context(), coderws.MessageText, payload))
			select {
			case err := <-serverErr:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("a regional policy change did not stop the reused WS connection")
			}
			require.Empty(t, upstream.writes, "the denied turn must send zero upstream frames")
		})
	}
}

func TestRegionalAvailabilityExpiryCancellationAndReadIsolation(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "fixture"}}
	report := availabilityAdmissionPass(account.ID, "target")
	report.PlanID = 41
	replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
	reader, err := NewRegionalSchedulerReader(replica)
	require.NoError(t, err)
	require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, regionalAvailabilityDoc(t, account, []*AvailabilityReport{report})))
	first, err := reader.ReadAccountAvailability(t.Context(), account)
	require.NoError(t, err)
	first[0].Models[0].Decision.Status = "fail"
	first[0].Configured = false
	second, err := reader.ReadAccountAvailability(t.Context(), account)
	require.NoError(t, err)
	require.True(t, second[0].Configured)
	require.Equal(t, "pass", second[0].Models[0].Decision.Status, "a caller must not mutate the saved view")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.ReadAccountAvailability(canceled, account)
	require.ErrorIs(t, err, context.Canceled)
	// Publish an expired packet through the real envelope checks. Existing
	// state must not be replaced or have its revision floor reset.
	now := time.Now()
	require.Error(t, replica.Publish(t.Context(), "test", 2, now.Add(-time.Minute), now.Add(-time.Millisecond), regionalAvailabilityDoc(t, account, []*AvailabilityReport{report})))
	current, err := replica.Current()
	require.NoError(t, err)
	require.Equal(t, uint64(1), current.Revision)
	// A full empty v2 view is authoritative removal, not a primary fallback.
	raw, err := json.Marshal(RegionalSchedulerDocument{Version: 2, Buckets: []RegionalSchedulerBucket{}, Availability: []RegionalAccountAvailability{}})
	require.NoError(t, err)
	require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, raw))
	_, err = reader.ReadAccountAvailability(t.Context(), account)
	require.ErrorIs(t, err, ErrAvailabilityRouteChanged)
}
