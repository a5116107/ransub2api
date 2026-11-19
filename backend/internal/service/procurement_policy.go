package service

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrProcurementPolicyConflict = infraerrors.Conflict("GROUP_PROCUREMENT_POLICY_CONFLICT", "procurement policy changed; review the latest policy before saving")
var ErrProcurementPolicyInvalid = infraerrors.BadRequest("INVALID_GROUP_PROCUREMENT_POLICY", "procurement policy requires a complete supported version, the current revision, an exact nonempty interval, explicit sources for versions 2/3 and explicit buckets for version 3")

func procurementPolicyUpdate(raw json.RawMessage, current *ProcurementPolicy) (*ProcurementPolicy, *int64, error) {
	if len(raw) == 0 {
		return current.Clone(), nil, nil
	}
	if len(raw) > 32768 || !json.Valid(raw) {
		return nil, nil, ErrProcurementPolicyInvalid
	}
	fields := make(map[string]json.RawMessage, 6)
	fieldDecoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := fieldDecoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, ErrProcurementPolicyInvalid
	}
	for fieldDecoder.More() {
		token, err := fieldDecoder.Token()
		if err != nil {
			return nil, nil, ErrProcurementPolicyInvalid
		}
		key, ok := token.(string)
		if !ok || fields[key] != nil || (key != "version" && key != "revision" && key != "min_rate" && key != "max_rate" && key != "sources" && key != "buckets") {
			return nil, nil, ErrProcurementPolicyInvalid
		}
		var value json.RawMessage
		if fieldDecoder.Decode(&value) != nil {
			return nil, nil, ErrProcurementPolicyInvalid
		}
		fields[key] = value
	}
	if len(fields) < 4 || len(fields) > 6 || fields["version"] == nil || fields["revision"] == nil || fields["min_rate"] == nil || fields["max_rate"] == nil || bytes.Equal(bytes.TrimSpace(fields["revision"]), []byte("null")) {
		return nil, nil, ErrProcurementPolicyInvalid
	}
	var draft *ProcurementPolicy
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&draft); err != nil || draft.ValidateDraft() != nil {
		return nil, nil, ErrProcurementPolicyInvalid
	}
	if draft.Version == 1 && fields["sources"] != nil || draft.Version < 3 && fields["buckets"] != nil {
		return nil, nil, ErrProcurementPolicyInvalid
	}
	expected := int64(0)
	if current != nil {
		if current.Validate() != nil {
			return nil, nil, ErrProcurementPolicyInvalid
		}
		expected = current.Revision
	}
	if draft.Revision != expected {
		return nil, nil, ErrProcurementPolicyConflict
	}
	// Check concurrency first so an older range-only draft can review the
	// newly added source contract through the normal 409 recovery flow.
	// At the current revision, clearing sources still requires explicit v2 null.
	if current != nil && draft.Version < current.Version {
		return nil, nil, ErrProcurementPolicyInvalid
	}
	if expected == domain.MaxProcurementPolicyRevision {
		return nil, nil, ErrProcurementPolicyInvalid
	}
	return draft.WithRevision(expected + 1), &expected, nil
}

func procurementPolicyVetoReason(policy *ProcurementPolicy, catalog *domain.ProcurementCatalog, account *Account, rate float64) string {
	allowed, err := policy.ContainsRate(rate)
	if err != nil {
		return openAIProfitFilterReasonProcurementUnavailable
	}
	if !allowed {
		return openAIProfitFilterReasonProcurementRange
	}
	if account == nil {
		return openAIProfitFilterReasonProcurementUnavailable
	}
	allowed, err = policy.AllowsAccount(account.ID)
	if err != nil {
		return openAIProfitFilterReasonProcurementUnavailable
	}
	if !allowed {
		return openAIProfitFilterReasonProcurementSource
	}
	if policy != nil && policy.Buckets != nil && policy.Buckets.Codes != nil {
		if catalog.Validate() != nil {
			return openAIProfitFilterReasonProcurementCatalog
		}
		for _, code := range policy.Buckets.Codes {
			if !slices.ContainsFunc(catalog.Buckets, func(b domain.ProcurementBucket) bool { return b.Code == code }) {
				return openAIProfitFilterReasonProcurementCatalog
			}
		}
		bucket, err := catalog.ClassifyRate(rate)
		if err != nil {
			return openAIProfitFilterReasonProcurementCatalog
		}
		if bucket == nil || !slices.Contains(policy.Buckets.Codes, bucket.Code) {
			return openAIProfitFilterReasonProcurementBucket
		}
	}
	return ""
}

func procurementPolicyGroupReader(snapshot *SchedulerSnapshotService, repo GroupRepository, id int64) func(context.Context) (*Group, error) {
	if snapshot != nil {
		// This method already selects the regional snapshot or authoritative
		// primary repository. Do not add a regional database fallback.
		return func(ctx context.Context) (*Group, error) { return snapshot.GetGroupByIDLite(ctx, id) }
	}
	if repo != nil {
		return func(ctx context.Context) (*Group, error) { return repo.GetByIDLite(ctx, id) }
	}
	return nil
}

// Refresh only the procurement constraint. The request's sale price, margin
// threshold and pricing instant remain immutable. Never mutate a shared gate.
func openAIProfitControlLatestPolicyVetoReason(ctx context.Context, account *Account) (bool, string) {
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	if gate == nil || account == nil || gate.configUnavailable {
		return openAIProfitControlVetoReason(ctx, account)
	}
	deny := func() (bool, string) {
		openAIProfitControlObserverInstance.recordVeto(gate.groupID, gate.platform, gate.threshold, openAIProfitFilterReasonProcurementUnavailable)
		return true, openAIProfitFilterReasonProcurementUnavailable
	}
	if gate.procurementGroup == nil {
		if gate.procurementPolicy != nil {
			return deny()
		}
		return openAIProfitControlVetoReason(ctx, account)
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	group, err := gate.procurementGroup(readCtx)
	cancel()
	if err != nil || group == nil || group.ID != gate.groupID || group.Platform != gate.platform {
		return deny()
	}
	latest := *gate
	latest.procurementPolicy = group.ProcurementPolicy.Clone()
	latest.procurementCatalog = group.ProcurementCatalog.Clone()
	if latest.procurementPolicy != nil && latest.procurementPolicy.Validate() != nil {
		return deny()
	}
	return openAIProfitControlVetoReason(context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &latest), account)
}
