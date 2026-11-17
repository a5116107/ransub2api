package repository

import (
	"context"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Local capacity is independent of the last supplier observation. A late
// deferral cannot overwrite a newer observation, configuration or authorization.
func (r *accountRepository) DeferUpstreamBillingProbe(ctx context.Context, account *service.Account, denial *service.MonitoringProbeBudgetDenial) error {
	if account == nil || denial == nil || denial.RetryAt.IsZero() {
		return service.ErrAccountNilInput
	}
	payload, err := json.Marshal(denial)
	if err != nil {
		return err
	}
	credentials, err := json.Marshal(account.Credentials)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(account.Extra[service.UpstreamBillingProbeExtraKey])
	if err != nil {
		return err
	}
	provider, err := json.Marshal(account.Extra["upstream_billing_provider"])
	if err != nil {
		return err
	}
	var proxyID any
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	result, err := r.sql.ExecContext(ctx, `UPDATE accounts SET extra=COALESCE(extra,'{}'::jsonb)||jsonb_build_object($1::text,$2::jsonb)
 WHERE id=$3 AND deleted_at IS NULL AND platform=$4 AND type=$5 AND credentials=$6::jsonb
 AND proxy_id IS NOT DISTINCT FROM $7
 AND COALESCE(extra->'upstream_billing_probe','null'::jsonb)=$8::jsonb
 AND COALESCE(extra->'upstream_billing_provider','null'::jsonb)=$9::jsonb
 AND updated_at=$10`,
		service.UpstreamBillingProbeDeferralExtraKey, string(payload), account.ID, account.Platform, account.Type, string(credentials), proxyID, string(snapshot), string(provider), account.UpdatedAt)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return service.ErrUpstreamBillingProbeIdentityChanged
	}
	return nil
}
