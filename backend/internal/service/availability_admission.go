package service

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// AccountAvailabilityReader reads all rules, including paused rules, from one
// primary-database snapshot. An empty result means the account has no rules.
type AccountAvailabilityReader interface {
	ReadAccountAvailability(context.Context, *Account) ([]*AvailabilityReport, error)
}

var ErrAvailabilityRouteChanged = errors.New("availability account route changed")

// AvailabilityModelForAccount resolves the configured probe model in the same
// namespace as the final request body. The request itself is already mapped.
func AvailabilityModelForAccount(account *Account, requested string) string {
	model := account.GetMappedModel(requested)
	if account.IsOpenAI() && account.IsOAuth() {
		model = normalizeOpenAIModelForUpstream(account, model)
	}
	return model
}

func (s *OpenAIGatewayService) admitAvailabilityRequest(ctx context.Context, request *http.Request, account *Account) error {
	if s.availabilityReader == nil || ctx.Value(qualityProbeContextKey{}) != nil {
		return nil
	}
	if account == nil || account.ID <= 0 {
		return denyOpenAITurn("account_unavailable")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reports, err := s.availabilityReader.ReadAccountAvailability(ctx, account)
	if err != nil {
		if errors.Is(err, ErrAvailabilityRouteChanged) {
			return denyOpenAITurn("account_binding_changed")
		}
		return &OpenAITurnAdmissionError{Reason: "availability_state_unavailable", cause: err}
	}
	configured := false
	for _, report := range reports {
		if report == nil || report.AccountID != account.ID {
			return denyOpenAITurn("availability_state_unavailable")
		}
		configured = configured || report.Configured
	}
	if !configured {
		return nil
	}
	// Native WS and BPS have no matching observed wire scope yet. A successful
	// ordinary HTTP probe must not authorize those distinct sends.
	if request == nil {
		return denyOpenAITurn("availability_scope_unconfirmed")
	}
	payload, confirmed := availabilityRequestPayload(request)
	scope := availabilityScopeForRequest(request, payload)
	if !confirmed || scope == nil {
		return denyOpenAITurn("availability_scope_unconfirmed")
	}
	matched := false
	for _, report := range reports {
		if !report.Configured {
			continue
		}
		if report.EvaluatedAt.IsZero() || len(report.Models) == 0 {
			return denyOpenAITurn("availability_not_confirmed")
		}
		for _, state := range report.Models {
			if AvailabilityModelForAccount(account, state.ModelID) != scope.Model {
				continue
			}
			matched = true
			if !report.Enabled {
				return denyOpenAITurn("availability_rule_paused")
			}
			// Charge the read and payload parsing time against the database
			// clock, so latency and local clock skew cannot extend a pass.
			if state.Status != "pass" || state.StatusAt(report.EvaluatedAt.Add(time.Since(started))) != "pass" {
				return denyOpenAITurn("availability_not_confirmed")
			}
			if state.Decision == nil || state.Decision.Scope == nil || *state.Decision.Scope != *scope {
				return denyOpenAITurn("availability_scope_unconfirmed")
			}
		}
	}
	if !matched {
		return denyOpenAITurn("availability_model_unobserved")
	}
	return nil
}
