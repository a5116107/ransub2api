package service

import (
	"context"
	"time"
)

func procurementAvailabilityReader(snapshot *SchedulerSnapshotService, repo any) AccountAvailabilityReader {
	if snapshot != nil {
		if snapshot.localSnapshotReader != nil {
			reader, _ := snapshot.localSnapshotReader.(AccountAvailabilityReader)
			return reader
		}
		repo = snapshot.groupRepo
	}
	reader, _ := repo.(AccountAvailabilityReader)
	return reader
}

func procurementAvailabilityRequired(gate *openAIProfitControlGate) bool {
	return gate != nil && gate.procurementPolicy != nil && gate.procurementPolicy.Availability == "current_state"
}

type procurementAvailabilityCacheEntry struct {
	evidence  *AvailabilityScopeEvidence
	expiresAt time.Time
}

func procurementAvailabilityScopeVetoReason(ctx context.Context, gate *openAIProfitControlGate, account *Account, scope *AvailabilityRequestScope) string {
	if !procurementAvailabilityRequired(gate) {
		return ""
	}
	// Scope comes from the original wire parser. Billing's scope constructor
	// excludes native protocols and private connection identity; using it here
	// would reject even an exact current observation for those transports.
	if gate.procurementPolicy.Validate() != nil || account == nil || scope == nil || gate.availabilityReader == nil {
		return "procurement_availability_unavailable"
	}
	key := procurementEvidenceCacheKey{AccountID: account.ID, Route: AccountRouteFingerprint(account), Scope: *scope}
	var entry procurementAvailabilityCacheEntry
	if gate.availabilityCache != nil {
		if cached, ok := gate.availabilityCache.Load(key); ok {
			entry, _ = cached.(procurementAvailabilityCacheEntry)
		}
	}
	if entry.evidence == nil {
		started := time.Now()
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		reports, err := gate.availabilityReader.ReadAccountAvailability(readCtx, account)
		cancel()
		if err != nil {
			return "procurement_availability_unavailable"
		}
		var at time.Time
		for _, report := range reports {
			if report != nil && report.EvaluatedAt.After(at) {
				at = report.EvaluatedAt
			}
		}
		entry.evidence = AssessAvailabilityScopeEvidence(account, scope, reports, at.Add(time.Since(started)))
		if entry.evidence.ValidUntil != nil {
			// Use the authority's elapsed validity, not the host's wall clock.
			// Charge the complete read to it; a slow read never extends a pass.
			entry.expiresAt = started.Add(entry.evidence.ValidUntil.Sub(at))
		}
		if gate.availabilityCache != nil {
			gate.availabilityCache.Store(key, entry)
		}
	}
	if !entry.expiresAt.After(time.Now()) {
		return "procurement_availability_unavailable"
	}
	return procurementAvailabilityReason(entry.evidence, entry.evidence.EvaluatedAt)
}

func procurementAvailabilityReason(evidence *AvailabilityScopeEvidence, at time.Time) string {
	if evidence == nil || evidence.Version != 1 || evidence.EvaluatedAt.IsZero() || !evidence.EvaluatedAt.Equal(at) || evidence.ValidUntil == nil || !evidence.ValidUntil.After(at) {
		return "procurement_availability_unavailable"
	}
	if evidence.Status == "fail" {
		return "procurement_availability_failed"
	}
	if evidence.Status != "pass" || evidence.Reason != "all_rules_passed" || evidence.RuleCount < 1 || evidence.ConfirmedRules != evidence.RuleCount {
		return "procurement_availability_unavailable"
	}
	return ""
}

func procurementAvailabilityCandidateVetoReason(ctx context.Context, account *Account) string {
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	if !procurementAvailabilityRequired(gate) {
		return ""
	}
	if gate.availabilityReader == nil {
		return "procurement_availability_unavailable"
	}
	if actual, present := ctx.Value(procurementActualScopeKey{}).(func() *AvailabilityRequestScope); present {
		fresh := *gate
		fresh.availabilityCache = nil
		prepared, _ := ctx.Value(procurementPreparedAccountKey{}).(*Account)
		if prepared != nil {
			account = prepared
		}
		return procurementAvailabilityScopeVetoReason(ctx, &fresh, account, actual())
	}
	return upstreamCandidateScopeVetoReason(ctx, account, func(scope *AvailabilityRequestScope) string {
		if scope == nil {
			return "" // Unpredictable conversion requires physical-send verification.
		}
		return procurementAvailabilityScopeVetoReason(ctx, gate, account, scope)
	})
}

type ProcurementAvailabilityPreview struct {
	Version  int    `json:"version"`
	Required bool   `json:"required"`
	State    string `json:"state"`
}

func ProcurementAvailability(policy *ProcurementPolicy, evidence *AvailabilityScopeEvidence, at time.Time) *ProcurementAvailabilityPreview {
	out := &ProcurementAvailabilityPreview{Version: 1, State: "not_required"}
	if policy == nil || policy.Availability != "current_state" {
		return out
	}
	out.Required = true
	switch procurementAvailabilityReason(evidence, at) {
	case "":
		out.State = "pass"
	case "procurement_availability_failed":
		out.State = "fail"
	default:
		out.State = "unknown"
	}
	return out
}
