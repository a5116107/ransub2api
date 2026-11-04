//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/stretchr/testify/require"
)

func TestAvailabilityScopeMatchesOnlyObservedCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, path, payload string
		allowed             bool
	}{
		{"same", "/v1/responses", `{"model":"target","input":"different prompt","stream":true,"service_tier":"priority","reasoning":{"effort":"high"}}`, true},
		{"different tier", "/v1/responses", `{"model":"target","input":"hi","stream":true,"service_tier":"default","reasoning":{"effort":"high"}}`, false},
		{"different effort", "/v1/responses", `{"model":"target","input":"hi","stream":true,"service_tier":"priority","reasoning":{"effort":"low"}}`, false},
		{"different stream", "/v1/responses", `{"model":"target","input":"hi","stream":false,"service_tier":"priority","reasoning":{"effort":"high"}}`, false},
		{"different protocol", "/v1/chat/completions", `{"model":"target","messages":[{"role":"user","content":"hi"}],"stream":true,"service_tier":"priority","reasoning_effort":"high"}`, false},
		{"tool", "/v1/responses", `{"model":"target","tools":[{"type":"function","name":"fixture"}]}`, false},
		{"image", "/v1/responses", `{"model":"target","input":[{"role":"user","content":[{"type":"input_image","image_url":"fixture"}]}]}`, false},
		{"unknown field", "/v1/responses", `{"model":"target","unknown_capability":true}`, false},
		{"unknown query", "/v1/responses?fast=true", `{"model":"target"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			report := availabilityAdmissionPass(account.ID, "target")
			report.Models[0].Decision.Scope.Stream = true
			report.Models[0].Decision.Scope.ServiceTier = "priority"
			report.Models[0].Decision.Scope.ReasoningEffort = "high"
			svc := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid"+tc.path, strings.NewReader(tc.payload))
			require.NoError(t, err)
			err = svc.admitAvailabilityRequest(request.Context(), request, account)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err))
			}
		})
	}
}

func TestAvailabilityProbeScopeRequiresActualRequestAssociation(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		name := "direct"
		if redirect {
			name = "redirect"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if redirect && req.URL.Path == "/v1/responses" {
					http.Redirect(w, req, "/other/responses", http.StatusTemporaryRedirect)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)
			collector := requesttiming.New(time.Now(), 0)
			observation := &availabilityProbeObservation{}
			ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
			payload := `{"model":"target","input":"hi","stream":true}`
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(payload))
			require.NoError(t, err)
			upstream := timedAccountTestUpstream(&runtimeFallbackUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return server.Client().Do(req)
			}})
			response, err := upstream.Do(request, "", 901, 1)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			if redirect {
				require.Nil(t, observation.scope, "a redirect must not attest to the original wire scope")
			} else {
				require.Equal(t, &AvailabilityRequestScope{Version: 1, Protocol: "responses_http", Stream: true, Model: "target", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}, observation.scope)
				require.Equal(t, 1, observation.scopeAttempt)
			}
		})
	}
}

func TestAvailabilityRoundScopeRequiresAllSamplesToMatch(t *testing.T) {
	for _, kind := range []string{"same", "missing", "different"} {
		t.Run(kind, func(t *testing.T) {
			plan := pelicanPlan()
			plan.PelicanConfig.ParallelCount = 2
			first := availabilityAdmissionPass(901, plan.ModelID).Models[0].Decision
			second := *first
			copy := *first.Scope
			second.Scope = &copy
			if kind == "missing" {
				second.Scope = nil
			}
			if kind == "different" {
				second.Scope.ServiceTier = "priority"
			}
			outcomes := availabilityRoundOutcomes(plan, []*ScheduledTestResult{
				{PelicanConfig: &PelicanTestConfig{ModelID: plan.ModelID, AvailabilityResult: first}},
				{PelicanConfig: &PelicanTestConfig{ModelID: plan.ModelID, AvailabilityResult: &second}},
			})
			require.Len(t, outcomes, 1)
			require.Equal(t, "pass", outcomes[0].Probe.Status)
			if kind == "same" {
				require.Equal(t, first.Scope, outcomes[0].Probe.Scope)
			} else {
				require.Nil(t, outcomes[0].Probe.Scope)
			}
		})
	}
}

func TestAvailabilityWSBindingRereadsRulesForNewTurns(t *testing.T) {
	account := wsSSETestAccount()
	repo := &availabilityAdmissionRepo{}
	svc := &OpenAIGatewayService{availabilityReader: repo}
	binding := svc.bindOpenAIWSHandshake(account, "gpt-5.1", make(http.Header))
	require.NoError(t, svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding), "unconfigured accounts keep the original behavior")
	repo.reports = []*AvailabilityReport{availabilityAdmissionPass(account.ID, "gpt-5.1")}
	require.True(t, IsOpenAITurnAdmissionError(svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding)), "a new policy must apply to an already bound connection")
	repo.reports = nil
	require.NoError(t, svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding), "explicit policy removal restores legacy eligibility")
	require.Equal(t, 3, repo.reads)
}
