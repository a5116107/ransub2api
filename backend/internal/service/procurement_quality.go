package service

import (
	"context"
	"net/http"
	"time"
)

// The existing SQL evidence reader owns rule/route/fault currentness. Regions
// without this authority fail closed rather than calling the primary database.
type QualityScopeEvidenceReader interface {
	ReadQualityScopeEvidence(context.Context, *Account, AvailabilityRequestScope) (*QualityScopeEvidence, error)
}

func procurementQualityReader(snapshot *SchedulerSnapshotService, repo GroupRepository) QualityScopeEvidenceReader {
	if snapshot != nil {
		if snapshot.localSnapshotReader != nil {
			reader, _ := snapshot.localSnapshotReader.(QualityScopeEvidenceReader)
			return reader
		}
		repo = snapshot.groupRepo
	}
	reader, _ := repo.(QualityScopeEvidenceReader)
	return reader
}

func procurementQualityRequired(gate *openAIProfitControlGate) bool {
	return gate != nil && gate.procurementPolicy != nil && gate.procurementPolicy.Quality == "current_answer"
}

type procurementQualityCacheKey struct {
	AccountID int64
	Route     [32]byte
	Scope     AvailabilityRequestScope
}

func procurementQualityScopeVetoReason(ctx context.Context, gate *openAIProfitControlGate, account *Account, scope *AvailabilityRequestScope) string {
	if !procurementQualityRequired(gate) {
		return ""
	}
	if scope != nil {
		normalized := *scope
		normalized.ConnectionScope = ""
		scope = &normalized
	}
	if gate.procurementPolicy.Validate() != nil || account == nil || scope == nil || !billingApplicabilityScopeShapeKnown(*scope) || gate.qualityReader == nil {
		return "procurement_quality_unavailable"
	}
	key := procurementQualityCacheKey{AccountID: account.ID, Route: AccountRouteFingerprint(account), Scope: *scope}
	var evidence *QualityScopeEvidence
	var expiresAt time.Time
	if gate.qualityCache != nil {
		if cached, ok := gate.qualityCache.Load(key); ok {
			if entry, valid := cached.(procurementQualityCacheEntry); valid {
				evidence, expiresAt = entry.evidence, entry.expiresAt
			}
		}
	}
	if evidence == nil {
		startedAt := time.Now()
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var err error
		evidence, err = gate.qualityReader.ReadQualityScopeEvidence(readCtx, account, *scope)
		cancel()
		if err != nil || evidence == nil {
			return "procurement_quality_unavailable"
		}
		if evidence.ValidUntil != nil {
			expiresAt = startedAt.Add(evidence.ValidUntil.Sub(evidence.EvaluatedAt))
		}
		if gate.qualityCache != nil {
			gate.qualityCache.Store(key, procurementQualityCacheEntry{evidence: evidence, expiresAt: expiresAt})
		}
	}
	if !expiresAt.After(time.Now()) {
		return "procurement_quality_unavailable"
	}
	return procurementAnswerQualityReason(evidence, evidence.EvaluatedAt)
}

type procurementQualityCacheEntry struct {
	evidence  *QualityScopeEvidence
	expiresAt time.Time
}

func procurementAnswerQualityReason(evidence *QualityScopeEvidence, at time.Time) string {
	if evidence == nil || evidence.Version != 1 || evidence.EvaluatedAt.IsZero() || !evidence.EvaluatedAt.Equal(at) || evidence.ValidUntil == nil || !evidence.ValidUntil.After(at) {
		return "procurement_quality_unavailable"
	}
	if evidence.Status == "fail" {
		return "procurement_quality_failed"
	}
	if evidence.Status != "pass" || evidence.Reason != "all_rules_passed" || evidence.RuleCount < 1 || evidence.ConfirmedRules != evidence.RuleCount || evidence.Samples < 1 || evidence.ValidSamples != evidence.Samples {
		return "procurement_quality_unavailable"
	}
	return ""
}

func procurementQualityCandidateVetoReason(ctx context.Context, account *Account) string {
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	if !procurementQualityRequired(gate) {
		return ""
	}
	if actual, present := ctx.Value(procurementQualityActualScopeKey{}).(func() *AvailabilityRequestScope); present {
		fresh := *gate
		fresh.qualityCache = nil
		prepared, _ := ctx.Value(procurementQualityPreparedAccountKey{}).(*Account)
		if prepared != nil {
			account = prepared
		}
		return procurementQualityScopeVetoReason(ctx, &fresh, account, actual())
	}
	return upstreamCandidateScopeVetoReason(ctx, account, func(scope *AvailabilityRequestScope) string {
		if scope == nil {
			return "" // Unknown conversion is checked at the actual send boundary.
		}
		return procurementQualityScopeVetoReason(ctx, gate, account, scope)
	})
}

type ProcurementAnswerQualityPreview struct {
	Version  int    `json:"version"`
	Required bool   `json:"required"`
	State    string `json:"state"`
}

func ProcurementAnswerQuality(policy *ProcurementPolicy, evidence *QualityScopeEvidence, at time.Time) *ProcurementAnswerQualityPreview {
	out := &ProcurementAnswerQualityPreview{Version: 1, State: "not_required"}
	if policy == nil || policy.Quality != "current_answer" {
		return out
	}
	out.Required = true
	switch procurementAnswerQualityReason(evidence, at) {
	case "":
		out.State = "pass"
	case "procurement_quality_failed":
		out.State = "fail"
	default:
		out.State = "unknown"
	}
	return out
}

// A present nil scope is an unconfirmed physical request, not permission to
// fall back to candidate metadata or a previous WS turn.
type procurementQualityActualScopeKey struct{}
type procurementQualityPreparedAccountKey struct{}

func withProcurementQualityHTTPRequest(request *http.Request, account *Account) *http.Request {
	ctx := request.Context()
	if !gatewayProfitControlGateActive(ctx) {
		return request
	}
	ctx = context.WithValue(ctx, procurementQualityPreparedAccountKey{}, account)
	ctx = context.WithValue(ctx, procurementQualityActualScopeKey{}, func() *AvailabilityRequestScope {
		proxy := ""
		if account != nil && account.Proxy != nil {
			proxy = account.Proxy.URL()
		}
		if payload, ok := availabilityRequestPayload(request); ok && qualityRouteForRequest(account, request, proxy) != nil {
			return availabilityScopeForRequest(request, payload)
		}
		return nil
	})
	// The lazy reader must restore the same request body the transport will send,
	// not an earlier shallow copy that would leave the outbound body consumed.
	request = request.WithContext(ctx)
	return request
}
