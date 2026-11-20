package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// These are the four non-form membership writers in this repository. Read the
// same authorities as their executors, without running them or treating saved
// audit markers as exclusions. The caller owns the repeatable-read transaction.
func loadProcurementWriterReview(ctx context.Context, tx sqlQueryer, product *service.Group, candidates []int64) (*service.ProcurementWriterReview, error) {
	out := &service.ProcurementWriterReview{Version: 1, Scope: "supply_candidates_and_explicit_writer_targets", InventoryComplete: true, Writers: []service.ProcurementWriter{}}
	var astra config.AstraRoutingSettings
	oauth := service.DefaultOAuthAutoConfig()
	load := func(key, kind string, target any, validate func() error) (bool, error) {
		var raw []byte
		err := scanSingleRow(ctx, tx, `SELECT value FROM settings WHERE key=$1`, []any{key}, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil // neither persisted executor can act without settings
		}
		if err != nil {
			return false, err
		}
		if procurementWriterObject(raw, target) != nil || validate() != nil {
			out.InventoryComplete = false // unknown settings may hide further targets
			out.Writers = append(out.Writers, service.ProcurementWriter{Kind: kind, Risk: "unknown", Reasons: []string{"configuration_unreadable"}})
			return false, nil
		}
		return true, nil
	}
	astraKnown, err := load("astra_routing_experiment_v1", "astra_scheduling", &astra, func() error { return astra.Validate() })
	if err != nil {
		return nil, err
	}
	// The runtime type has a custom UnmarshalJSON. Decode its identical plain
	// shape so that DisallowUnknownFields still applies to this safety review.
	type oauthWriterSettings service.OAuthAutoConfig
	oauthKnown, err := load(service.SettingKeyOAuthAutoConfig, "oauth_degradation", (*oauthWriterSettings)(&oauth), func() error { return oauth.DegradationAction.Validate() })
	if err != nil {
		return nil, err
	}
	// A current explicit destination or saved restore can refer to this product
	// even after the account left every approved source. Such accounts are read
	// for conflicts only; they never become approved supply or price candidates.
	astraTargets := []int64{}
	astraBroad := false
	if astraKnown && (astra.AccountScheduling || astra.TicketPoolScheduling) && astra.EffectiveSchedulingMode() == "groups" && slices.Contains(astra.SchedulingGroupIDs, product.ID) {
		astraTargets = astra.CookiePool.TargetAccountIDs
		astraBroad = astra.TicketPoolScheduling
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.name,a.platform,a.type,a.parent_account_id,a.credentials,COALESCE(a.extra,'{}'::jsonb),s.state,p.enabled,p.pelican_config
 FROM accounts a LEFT JOIN astra_scheduling_states s ON s.account_id=a.id
 LEFT JOIN scheduled_test_plans p ON p.account_id=a.id AND p.id::text=a.extra#>>'{openai_two_fa_admission,plan_id}'
 WHERE a.deleted_at IS NULL AND (
 a.id=ANY($1) OR a.extra->>'openai_excel_bps_403_target_group_id'=$2::text
 OR a.extra#>>'{openai_two_fa_admission,policy,isolation_group_id}'=$2::text
 OR a.extra#>'{oauth_degradation_action,group_ids}' @> jsonb_build_array($3::bigint)
 OR a.extra#>'{oauth_degradation_action,removed_group_ids}' @> jsonb_build_array($3::bigint)
 OR s.state->'group_ids' @> jsonb_build_array($3::bigint)
 OR s.state->'removed' @> jsonb_build_array(jsonb_build_object('group_id',$3::bigint))
 OR s.state->'memberships' @> jsonb_build_array(jsonb_build_object('group_id',$3::bigint))
 OR a.id=ANY($4) OR ($5 AND a.platform='openai' AND a.type IN ('oauth','setup-token') AND a.parent_account_id IS NULL))
 ORDER BY a.id LIMIT $6`, pq.Array(candidates), fmt.Sprint(product.ID), product.ID, pq.Array(astraTargets), astraBroad, service.ProcurementProductImpactAccountLimit+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if out.AccountsExamined == service.ProcurementProductImpactAccountLimit {
			return nil, service.ErrProcurementProductImpactTooLarge
		}
		var a service.Account
		var parent sql.NullInt64
		var credentials, extra, state, planRaw []byte
		var planEnabled sql.NullBool
		if err := rows.Scan(&a.ID, &a.Name, &a.Platform, &a.Type, &parent, &credentials, &extra, &state, &planEnabled, &planRaw); err != nil {
			return nil, err
		}
		out.AccountsExamined++
		if parent.Valid {
			a.ParentAccountID = &parent.Int64
		}
		base := service.ProcurementWriter{AccountID: &a.ID, AccountName: a.Name}
		if json.Unmarshal(credentials, &a.Credentials) != nil || json.Unmarshal(extra, &a.Extra) != nil {
			base.Kind, base.Risk, base.Reasons = "account_configuration", "unknown", []string{"configuration_unreadable"}
			out.Writers = append(out.Writers, base)
			continue
		}
		if row := procurementExcelWriter(a, base); row != nil {
			out.Writers = append(out.Writers, *row)
		}
		if row := procurementOAuthWriter(a, base, product.ID, oauth, oauthKnown); row != nil {
			out.Writers = append(out.Writers, *row)
		}
		if row := procurementAstraWriter(a, base, product.ID, astra, astraKnown, state); row != nil {
			out.Writers = append(out.Writers, *row)
		}
		if row := procurementAdmissionWriter(a, base, planEnabled, planRaw); row != nil {
			out.Writers = append(out.Writers, *row)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for _, row := range out.Writers {
		switch row.Risk {
		case "membership_conflict":
			out.MembershipConflicts++
		case "account_protection":
			out.AccountProtections++
		case "historical_only":
			out.HistoricalOnly++
		case "unknown":
			out.Unknown++
		}
	}
	return out, nil
}

func procurementWriterObject(raw []byte, target any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("writer configuration is not an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing writer configuration")
	}
	return nil
}

func procurementWriterFlag(value bool) *bool { return &value }

func procurementExcelWriter(a service.Account, row service.ProcurementWriter) *service.ProcurementWriter {
	row.Kind = "excel_bps_403"
	auto, exists := a.Extra[service.ExcelBPSAutoMoveOn403Key]
	if !exists || auto == false {
		return nil
	}
	if auto != true {
		row.Risk, row.Reasons = "unknown", []string{"configuration_unreadable"}
		return &row
	}
	row.Enabled = procurementWriterFlag(false)
	if !a.IsExcelBPSEnabled() {
		row.Risk, row.Reasons = "historical_only", []string{"not_applicable"}
		return &row
	}
	_, valid := a.ExcelBPS403GroupTarget()
	if !valid {
		row.Enabled, row.Risk, row.Reasons = nil, "unknown", []string{"configuration_unreadable"}
		return &row
	}
	row.Enabled, row.Risk, row.WritesProduct, row.Reasons = procurementWriterFlag(true), "membership_conflict", true, []string{"moves_all_memberships"}
	return &row
}

func procurementOAuthWriter(a service.Account, row service.ProcurementWriter, productID int64, cfg service.OAuthAutoConfig, known bool) *service.ProcurementWriter {
	row.Kind = "oauth_degradation"
	if raw, saved := a.Extra[service.OAuthDegradationActionExtraKey]; saved {
		encoded, err := json.Marshal(raw)
		var state service.OAuthDegradationState
		row.Enabled = procurementWriterFlag(false)
		if err != nil || procurementWriterObject(encoded, &state) != nil || state.Revision == "" {
			row.Enabled, row.Risk, row.Reasons = nil, "unknown", []string{"saved_state_unreadable"}
		} else {
			row.Risk, row.Reasons = "historical_only", []string{"native_history_not_exclusion"}
		}
		return &row
	}
	if !known || !cfg.DegradationAction.Enabled || a.Type != service.AccountTypeOAuth || a.ParentAccountID != nil {
		return nil
	}
	row.Enabled = procurementWriterFlag(true)
	if cfg.DegradationAction.LimitGroups && slices.Contains(cfg.DegradationAction.GroupIDs, productID) {
		row.Risk, row.WritesProduct, row.Reasons = "membership_conflict", true, []string{"removes_product_on_trigger"}
	} else {
		row.Risk, row.Reasons = "account_protection", []string{"native_account_controls"}
	}
	return &row
}

func procurementAstraWriter(a service.Account, row service.ProcurementWriter, productID int64, cfg config.AstraRoutingSettings, known bool, raw []byte) *service.ProcurementWriter {
	row.Kind = "astra_scheduling"
	active := known && (cfg.AccountScheduling || cfg.TicketPoolScheduling) && a.Platform == service.PlatformOpenAI && a.IsOpenAIOAuthLike() && a.ParentAccountID == nil &&
		(cfg.TicketPoolScheduling || slices.Contains(cfg.CookiePool.TargetAccountIDs, a.ID))
	var state astraActionState
	if len(raw) > 0 {
		if procurementWriterObject(raw, &state) != nil || !validProcurementAstraState(a.ID, state) {
			row.Risk, row.Reasons = "unknown", []string{"saved_state_unreadable"}
			return &row
		}
	}
	row.RetainedOwnership = state.Mode != "" || len(state.GroupIDs)+len(state.Removed)+len(state.Memberships) > 0
	if !active && !row.RetainedOwnership {
		return nil
	}
	if !known {
		row.Risk, row.Reasons = "unknown", []string{"configuration_unreadable"}
		return &row
	}
	row.Enabled = procurementWriterFlag(active)
	row.WritesProduct = active && cfg.EffectiveSchedulingMode() == "groups" && slices.Contains(cfg.SchedulingGroupIDs, productID)
	// Native account/model actions retain selection metadata too. Only group
	// mode owns memberships; an old selection is not a competing group owner.
	ownedProduct := state.Mode == "groups" && (slices.Contains(state.GroupIDs, productID) || slices.ContainsFunc(state.Removed, func(g qualityGroup) bool { return g.GroupID == productID }) ||
		slices.ContainsFunc(state.Memberships, func(m astraGroupMembership) bool { return m.GroupID == productID }))
	if row.WritesProduct || ownedProduct {
		row.Risk, row.Reasons = "membership_conflict", []string{"astra_membership_owner"}
	} else {
		row.Risk, row.Reasons = "account_protection", []string{"native_account_controls"}
	}
	return &row
}

func validProcurementAstraState(accountID int64, state astraActionState) bool {
	if !slices.Contains([]string{"", "groups", "account", "model"}, state.Mode) || len(state.GroupIDs) > 100 || len(state.Memberships) > 100 || len(state.Removed) > 100 ||
		state.Mode != "groups" && len(state.Memberships)+len(state.Removed) > 0 || state.Mode == "" && len(state.GroupIDs) > 0 {
		return false
	}
	validGroup := func(g *qualityGroup, groupID int64) bool {
		return g == nil || g.GroupID == groupID && (g.AccountID == 0 || g.AccountID == accountID)
	}
	ids, memberships, removed := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, id := range state.GroupIDs {
		if id < 1 || ids[id] {
			return false
		}
		ids[id] = true
	}
	for _, m := range state.Memberships {
		if m.GroupID < 1 || memberships[m.GroupID] || !validGroup(m.Before, m.GroupID) || !validGroup(m.After, m.GroupID) {
			return false
		}
		memberships[m.GroupID] = true
	}
	for _, g := range state.Removed {
		if g.GroupID < 1 || removed[g.GroupID] || !validGroup(&g, g.GroupID) {
			return false
		}
		removed[g.GroupID] = true
	}
	return true
}

func procurementAdmissionWriter(a service.Account, row service.ProcurementWriter, enabled sql.NullBool, planRaw []byte) *service.ProcurementWriter {
	raw, exists := a.Extra[service.OpenAITwoFAAdmissionExtraKey]
	if !exists {
		return nil
	}
	row.Kind = "two_fa_admission"
	var state service.OpenAITwoFAAdmissionState
	encoded, err := json.Marshal(raw)
	if err != nil || procurementWriterObject(encoded, &state) != nil || state.PlanID < 1 || state.Policy.Validate() != nil ||
		!slices.Contains([]string{"pending", "passed", "isolated", "isolation_pending"}, state.Phase) {
		row.Risk, row.Reasons = "unknown", []string{"saved_state_unreadable"}
		return &row
	}
	var plan *service.PelicanTestConfig
	if !enabled.Valid || json.Unmarshal(planRaw, &plan) != nil || plan == nil || plan.Quality == nil || plan.Quality.Action != service.QualityActionOpenAITwoFAAdmission ||
		plan.Quality.Admission == nil || !reflect.DeepEqual(state.Policy, *plan.Quality.Admission) {
		row.Risk, row.Reasons = "unknown", []string{"admission_plan_mismatch"}
		return &row
	}
	row.Enabled = procurementWriterFlag(enabled.Bool)
	if a.Platform != service.PlatformOpenAI || a.Type != service.AccountTypeOAuth || a.ParentAccountID != nil || state.Phase == "isolated" {
		row.Risk, row.Reasons = "historical_only", []string{"native_history_not_exclusion"}
	} else if enabled.Bool {
		row.Risk, row.WritesProduct, row.Reasons = "membership_conflict", true, []string{"moves_all_memberships"}
	} else {
		row.Risk, row.Reasons = "account_protection", []string{"paused_enrollment_can_resume"}
	}
	return &row
}
