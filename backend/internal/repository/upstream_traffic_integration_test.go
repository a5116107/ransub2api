//go:build integration

package repository

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUpstreamTrafficSourcesSplitAttemptsAndFinalCache(t *testing.T) {
	f := newChannelOperationsFixture(t)
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC().Truncate(time.Second)
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Minute), Until: now.Add(time.Minute), AccountID: f.account,
		GroupID: f.group, Model: "public-model", ModelScope: "public", Dimension: "account", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM request_timing_details WHERE usage_log_id IN (SELECT id FROM usage_logs WHERE api_key_id=$1)`, f.key)
		_, _ = integrationDB.Exec(`DELETE FROM request_connection_traces WHERE api_key_id=$1`, f.key)
	})
	cases := []string{
		`{"version":1,"source":"gateway_ingress","kind":"gateway"}`,
		`{"version":1,"source":"account_connection_test","kind":"synthetic"}`,
		`null`, // No history reconstruction from a path, session or billing row.
		`{"version":9,"source":"gateway_ingress","kind":"gateway"}`,
		`{"version":"1","source":"gateway_ingress","kind":"gateway"}`,
		`{"version":1,"source":"gateway_ingress","kind":"synthetic"}`,
		`{"version":1,"source":"account_connection_test","kind":"gateway"}`,
		`{"version":1,"source":"client_declared","kind":"gateway"}`,
		`[]`, `"gateway_ingress"`,
	}
	for i, marker := range cases {
		log := &service.UsageLog{UserID: f.user, APIKeyID: f.key, AccountID: f.account, GroupID: &f.group,
			Model: "billing-model", RequestedModel: "public-model", RequestID: uuid.NewString(), CreatedAt: now}
		_, err := repo.Create(t.Context(), log)
		require.NoError(t, err)
		end := 100.0
		read := int64(20)
		if i == 1 {
			read = 100
		}
		s := requesttiming.Snapshot{Version: 1, TraceID: uuid.NewString(), StartedAt: now, TotalMS: end, Status: 200,
			Outcome: "success", Terminal: "completed", FinalResponseAttempt: 1,
			Attempts: []requesttiming.Attempt{{Kind: "http", Number: 1, AccountID: f.account, EndMS: &end, Status: 200, Terminal: "completed",
				Route: transportdiag.Route{Endpoint: "responses"}, RouteFingerprint: strings.Repeat("b", 64),
				UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("a", 64)},
				UpstreamRequest:    &requesttiming.UpstreamRequestObservation{Version: 1, Status: "observed", Source: "http_transport_body", Protocol: "responses", Model: "wire-model", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Stream: true},
				CacheUsage:         &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: read}}}}
		require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: log.RequestID, apiKeyID: f.key, data: s}))
		require.NoError(t, repo.writeConnectionTrace(t.Context(), service.ConnectionTrace{Source: "application", TraceID: s.TraceID,
			StartedAt: now, RequestID: log.RequestID, APIKeyID: f.key, UserID: f.user, AccountID: f.account, GroupID: f.group,
			Model: "public-model", Path: "/v1/responses", Detail: s}))
		for _, table := range []string{"request_timing_details", "request_connection_traces"} {
			_, err = integrationDB.Exec(`UPDATE `+table+` SET detail=jsonb_set(detail,'{traffic_source}',$2::jsonb) WHERE trace_id=$1`, s.TraceID, marker)
			require.NoError(t, err)
		}
	}
	read := func() map[string]json.RawMessage {
		t.Helper()
		out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
		require.NoError(t, err)
		require.Equal(t, "ready", out.Attempts.Status, string(out.Attempts.Data))
		require.Equal(t, "ready", out.RawCache.Status, string(out.RawCache.Data))
		return map[string]json.RawMessage{"attempts": out.Attempts.Data, "cache": out.RawCache.Data}
	}
	before := read()
	for name, raw := range before {
		t.Run(name, func(t *testing.T) {
			var got struct {
				Total   int
				Traffic struct {
					Version  int
					Source   string
					Samples  int
					Known    int `json:"known_samples"`
					Unknown  int `json:"unknown_samples"`
					Coverage float64
					Rows     []struct {
						Source  string `json:"traffic_source"`
						Samples int
					}
				}
				Rows []struct {
					Source string  `json:"traffic_source"`
					Rate   float64 `json:"token_read_rate"`
					Cache  struct {
						Rate float64 `json:"token_read_rate"`
					} `json:"raw_cache"`
				}
			}
			require.NoError(t, json.Unmarshal(raw, &got))
			require.Equal(t, 3, got.Total, "identical Key/model/route must still split by observed source")
			require.Equal(t, 1, got.Traffic.Version)
			require.Equal(t, "server_recorded_entrypoints", got.Traffic.Source)
			require.Equal(t, 10, got.Traffic.Samples)
			require.Equal(t, 2, got.Traffic.Known)
			require.Equal(t, 8, got.Traffic.Unknown)
			require.Equal(t, .2, got.Traffic.Coverage)
			counts := map[string]int{}
			for _, row := range got.Traffic.Rows {
				counts[row.Source] = row.Samples
			}
			require.Equal(t, map[string]int{"gateway_ingress": 1, "account_connection_test": 1, "unknown": 8}, counts)
			for _, row := range got.Rows {
				expected := .2
				if row.Source == "account_connection_test" {
					expected = 1
				}
				if name == "attempts" {
					require.Equal(t, expected, row.Cache.Rate)
				} else {
					require.Equal(t, expected, row.Rate)
				}
			}
		})
	}
	// The same immutable origin must survive the legacy unique-usage fallback.
	_, err := integrationDB.Exec(`DELETE FROM request_connection_traces WHERE api_key_id=$1`, f.key)
	require.NoError(t, err)
	after := read()
	for name, raw := range before {
		var a, b map[string]any
		require.NoError(t, json.Unmarshal(raw, &a))
		require.NoError(t, json.Unmarshal(after[name], &b))
		require.Equal(t, a["traffic"], b["traffic"])
	}
	q.PageSize = 1
	for q.Page = 1; q.Page <= 3; q.Page++ {
		for _, raw := range read() {
			var got struct {
				Rows  []json.RawMessage
				Total int
			}
			require.NoError(t, json.Unmarshal(raw, &got))
			require.Len(t, got.Rows, 1)
			require.Equal(t, 3, got.Total)
		}
	}
}
