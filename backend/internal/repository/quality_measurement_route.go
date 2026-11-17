package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func marshalQualityResultConfig(result *service.ScheduledTestResult) any {
	if result.PelicanConfig == nil {
		return nil
	}
	var evidence *service.QualityRouteEvidence
	if result.QualityRouteEvidence.MatchesResult(result) {
		evidence = result.QualityRouteEvidence
	}
	raw, err := json.Marshal(struct {
		*service.PelicanTestConfig
		Route *service.QualityRouteEvidence `json:"_quality_observed_route,omitempty"`
	}{result.PelicanConfig, evidence})
	if err != nil {
		return nil
	}
	return string(raw)
}

type qualityRouteRead struct {
	assessment *service.QualityMeasurementRouteValidity
	cooldown   sql.NullTime
	observedAt time.Time
}

func readQualityRoute(ctx context.Context, tx *sql.Tx, accountID int64, result *service.ScheduledTestResult, raw []byte) (*qualityRouteRead, error) {
	out := &qualityRouteRead{assessment: &service.QualityMeasurementRouteValidity{Status: "unknown", Reason: "route_not_observed"}}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var evidence service.QualityRouteEvidence
	if json.Unmarshal(raw, &evidence) != nil || evidence.AccountID != accountID || !evidence.MatchesResult(result) {
		out.assessment.Reason = "invalid_route_evidence"
		return out, nil
	}
	out.observedAt = evidence.ObservedAt
	fingerprint, err := accountRouteFingerprint(ctx, tx, accountID, false)
	if errors.Is(err, sql.ErrNoRows) {
		out.assessment.Status, out.assessment.Reason = "unavailable", "account_unavailable"
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(fingerprint[:]) != evidence.Fingerprint {
		out.assessment.Status, out.assessment.Reason = "changed", "route_changed"
		return out, nil
	}
	managed, err := availabilityAccountManaged(ctx, tx, accountID)
	if err != nil {
		return nil, err
	}
	if managed != (evidence.ConnectionID != "") {
		out.assessment.Status, out.assessment.Reason = "changed", "connection_changed"
		return out, nil
	}
	if managed {
		capability := "responses"
		if result.PelicanConfig.QualityMeasurement.Scope.Protocol == "chat_http" {
			capability = "chat_completions"
		}
		var revision int64
		var kind, status, refresh string
		var capable bool
		err = tx.QueryRowContext(ctx, `SELECT revision,kind,status,refresh_state,cooldown_until,
 COALESCE(jsonb_typeof(capabilities)='array' AND capabilities ? $3,false)
 FROM openai_auth_connections WHERE id::text=$1 AND account_id=$2`, evidence.ConnectionID, accountID, capability).
			Scan(&revision, &kind, &status, &refresh, &out.cooldown, &capable)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (kind != "api_key" || revision != evidence.ConnectionRevision)) {
			out.assessment.Status, out.assessment.Reason = "changed", "connection_changed"
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if status != "active" || (refresh != "" && refresh != "idle") || !capable {
			out.assessment.Status, out.assessment.Reason = "unavailable", "connection_unavailable"
			return out, nil
		}
	}
	out.assessment.Status, out.assessment.Reason = "matching", "route_matches"
	return out, nil
}

func (read *qualityRouteRead) assess(now time.Time) *service.QualityMeasurementRouteValidity {
	if !read.observedAt.IsZero() && read.observedAt.After(now) {
		return &service.QualityMeasurementRouteValidity{Status: "unknown", Reason: "invalid_route_evidence"}
	}
	if read.assessment.Status == "matching" && read.cooldown.Valid && now.Before(read.cooldown.Time) {
		return &service.QualityMeasurementRouteValidity{Status: "unavailable", Reason: "connection_unavailable"}
	}
	return read.assessment
}
