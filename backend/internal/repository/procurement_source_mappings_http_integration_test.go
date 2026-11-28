//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProcurementSourceMappingHTTPPersistCASPrivacyAndRecovery(t *testing.T) {
	db, _ := repository.UpstreamChannelsTestDatabase(t)
	repo := repository.NewChannelRepository(db)
	h := admin.NewChannelHandler(service.NewChannelService(repo, nil, nil, nil, nil), nil, nil)
	router := gin.New()
	router.GET("/maps", h.ProcurementSourceMappings)
	router.GET("/maps/:id", h.ProcurementSourceMappings)
	router.POST("/maps", h.ProcurementSourceMappings)
	router.PUT("/maps/:id", h.ProcurementSourceMappings)
	router.DELETE("/maps/:id", h.ProcurementSourceMappings)
	router.PUT("/groups/:id", h.ProcurementSourceGroup)
	router.DELETE("/groups/:id", h.DeleteProcurementSourceGroup)
	router.PUT("/sites/:id", h.ProcurementSourceSite)
	router.DELETE("/sites/:id", h.ProcurementSourceSite)
	run := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, request)
		return w
	}
	var group, account int64
	require.NoError(t, db.QueryRow(`INSERT INTO groups(name,platform) VALUES('Source HTTP group','openai') RETURNING id`).Scan(&group))
	require.NoError(t, db.QueryRow(`INSERT INTO accounts(name,platform,type,credentials) VALUES('Source HTTP account','openai','apikey','{"api_key":"do-not-disclose-key","base_url":"https://private-source.example/v1","header_override_enabled":true,"header_overrides":{"X-Sub2API-Group-ID":"9007199254740993"}}') RETURNING id`).Scan(&account))
	_, err := db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account, group)
	require.NoError(t, err)
	var beforeOutbox int64
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox`).Scan(&beforeOutbox))
	body := fmt.Sprintf(`{"version":1,"site_code":"http-site","site_name":"HTTP Supplier","platform":"openai","external_group_id":"9007199254740993","external_group_name":"Fixed group","local_group_id":%d,"account_id":%d,"confirm":true}`, group, account)
	created := run("POST", "/maps", body)
	require.Equal(t, 200, created.Code, created.Body.String())
	var receipt struct {
		Data service.ProcurementSourceMapping `json:"data"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &receipt))
	item := receipt.Data
	require.Positive(t, item.ID)
	require.False(t, item.Dispatchable)
	require.Equal(t, 200, run("GET", fmt.Sprintf("/maps/%d", item.ID), "").Code)
	require.Equal(t, "9007199254740993", item.ExternalGroupID)
	require.Equal(t, "no-store", created.Header().Get("Cache-Control"))
	read := run("GET", "/maps?search=HTTP&platform=openai", "")
	require.Equal(t, 200, read.Code, read.Body.String())
	for _, private := range []string{"do-not-disclose-key", "private-source.example", "route_fingerprint", "credentials", "api_key"} {
		require.NotContains(t, read.Body.String(), private)
	}
	require.Contains(t, read.Body.String(), `"version":1`)
	require.Contains(t, read.Body.String(), `"conclusion":"pending_verification"`)
	require.Contains(t, read.Body.String(), `"external_group_id":"9007199254740993"`)
	require.Equal(t, "not_observed", item.Evidence.Selection.Status)
	// The same exact external ID is confirmed by a call-key billing receipt,
	// not by the unrelated local source Group's numeric ID or a visible list.
	at := time.Now().UTC().Add(-time.Minute)
	fresh := at.Add(time.Hour)
	snapshot := service.UpstreamBillingProbeSnapshot{Status: "ok", LastAttemptAt: at, ReceivedAt: &at, FreshUntil: &fresh,
		Data: map[string]any{"object": "sub2api.key_billing", "schema_version": 1, "billing_scope": "token", "selected_group_id": item.ExternalGroupID, "resolved_rate_multiplier": 0,
			"observed_at": at.Format(time.RFC3339Nano), "rate_applicability": map[string]any{"schema_version": 1, "source_version": "http-v1", "observed_at": at, "expires_at": fresh,
				"source": "private-discarded-source", "entries": []any{map[string]any{"model": "gpt-fixture", "protocol": "responses_http", "service_tier": "priority", "reasoning_effort": "high", "context": "all", "group_rate_applies": false, "conclusion": "covered", "private": "do-not-disclose-model-extra"}}}}}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(COALESCE(extra,'{}'::jsonb),'{upstream_billing_probe}',$2::jsonb) WHERE id=$1`, account, string(raw))
	require.NoError(t, err)
	var beforeRead, afterRead string
	require.NoError(t, db.QueryRow(`SELECT ctid::text||xmin::text||extra::text FROM accounts WHERE id=$1`, account).Scan(&beforeRead))
	read = run("GET", fmt.Sprintf("/maps/%d", item.ID), "")
	require.Equal(t, 200, read.Code)
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &receipt))
	require.Equal(t, "receipt_confirmed", receipt.Data.Evidence.Selection.Status)
	require.Equal(t, item.ExternalGroupID, receipt.Data.Evidence.Selection.GroupID)
	require.Equal(t, "declared", receipt.Data.Evidence.Models.Status)
	require.Equal(t, "excluded", receipt.Data.Evidence.Models.Entries[0].Conclusion, "conclusions are recomputed, never copied from the supplier")
	require.False(t, receipt.Data.Dispatchable)
	require.Equal(t, "unverified", receipt.Data.FixedGroupSelection)
	for _, private := range []string{"do-not-disclose-key", "private-source.example", "route_fingerprint", "private-discarded-source", "do-not-disclose-model-extra"} {
		require.NotContains(t, read.Body.String(), private)
	}
	require.NoError(t, db.QueryRow(`SELECT ctid::text||xmin::text||extra::text FROM accounts WHERE id=$1`, account).Scan(&afterRead))
	require.Equal(t, beforeRead, afterRead, "reading declarations must not persist rewritten conclusions or renew evidence")

	for _, bad := range []string{
		strings.Replace(body, `"confirm":true`, `"confirm":true,"confirmed":true`, 1),
		strings.Replace(body, `"external_group_id":"9007199254740993"`, `"external_group_id":9007199254740993`, 1),
		strings.Replace(body, `"confirm":true`, `"confirm":true,"confirm":true`, 1),
		strings.Replace(body, `"confirm":true`, `"confirm":true,"expected_revision":1`, 1),
	} {
		require.Equal(t, 400, run("POST", "/maps", bad).Code)
	}
	require.Equal(t, 409, run("POST", "/maps", body).Code)
	require.Equal(t, 409, run("DELETE", fmt.Sprintf("/groups/%d", item.SourceGroupID), fmt.Sprintf(`{"version":1,"expected_revision":%d,"confirm":true}`, item.SourceGroupRevision)).Code)
	require.Equal(t, 400, run("PUT", fmt.Sprintf("/maps/%d", item.ID), `{"version":1,"expected_revision":1,"enabled":null,"confirm":true}`).Code)

	// Two administrators editing the same revision get one success, one
	// conflict. The rejected edit never changes the committed state.
	path := fmt.Sprintf("/maps/%d", item.ID)
	command := fmt.Sprintf(`{"version":1,"expected_revision":%d,"enabled":false,"confirm":true}`, item.Revision)
	results := make(chan int, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() { defer wait.Done(); results <- run("PUT", path, command).Code }()
	}
	wait.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	require.Equal(t, map[int]int{200: 1, 409: 1}, counts)
	read = run("GET", "/maps", "")
	var page struct {
		Data service.ProcurementSourceMappingPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &page))
	require.Len(t, page.Data.Items, 1)
	item = page.Data.Items[0]
	require.EqualValues(t, 2, item.Revision)
	require.Equal(t, service.ProcurementSourceConclusionPaused, item.Conclusion)

	// A read failure is not an empty inventory or a successful write receipt.
	_, err = db.Exec(`ALTER TABLE procurement_supplier_sites RENAME TO hidden_supplier_sites`)
	require.NoError(t, err)
	failed := run("GET", "/maps", "")
	require.Equal(t, 503, failed.Code)
	require.NotContains(t, failed.Body.String(), "pq:")
	_, err = db.Exec(`ALTER TABLE hidden_supplier_sites RENAME TO procurement_supplier_sites`)
	require.NoError(t, err)
	require.Equal(t, 200, run("GET", "/maps", "").Code)

	renamed := run("PUT", fmt.Sprintf("/sites/%d", item.SourceSiteID), fmt.Sprintf(`{"version":1,"expected_revision":%d,"name":"Renamed supplier","confirm":true}`, item.SiteRevision))
	require.Equal(t, 200, renamed.Code, renamed.Body.String())
	read = run("GET", "/maps?search=Renamed", "")
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &page))
	require.Equal(t, "Renamed supplier", page.Data.Items[0].SiteName)
	require.Equal(t, 409, run("DELETE", fmt.Sprintf("/sites/%d", item.SourceSiteID), fmt.Sprintf(`{"version":1,"expected_revision":%d,"confirm":true}`, item.SiteRevision)).Code)

	require.Equal(t, 200, run("DELETE", path, `{"version":1,"expected_revision":2,"confirm":true}`).Code)
	require.Equal(t, 404, run("DELETE", path, `{"version":1,"expected_revision":2,"confirm":true}`).Code)
	read = run("GET", "/maps", "")
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &page))
	require.Len(t, page.Data.Items, 1)
	emptyGroup := page.Data.Items[0]
	require.Zero(t, emptyGroup.AccountID)
	require.Equal(t, service.ProcurementSourceConclusionBlocked, emptyGroup.Conclusion)
	require.Equal(t, 200, run("DELETE", fmt.Sprintf("/groups/%d", item.SourceGroupID), fmt.Sprintf(`{"version":1,"expected_revision":%d,"confirm":true}`, emptyGroup.SourceGroupRevision)).Code)
	// The remaining explicit site is removed only after every group is gone.
	require.Equal(t, 200, run("DELETE", fmt.Sprintf("/sites/%d", item.SourceSiteID), fmt.Sprintf(`{"version":1,"expected_revision":%d,"confirm":true}`, emptyGroup.SiteRevision+1)).Code)
	var afterOutbox int64
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox`).Scan(&afterOutbox))
	require.Equal(t, beforeOutbox, afterOutbox, "registry changes must not publish routing updates")
	var member bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM account_groups WHERE account_id=$1 AND group_id=$2)`, account, group).Scan(&member))
	require.True(t, member)
	// Scoped observers receive no private inventory even through the repository.
	_, err = repo.(service.ProcurementSourceMappingRepository).ListProcurementSourceMappings(service.WithObserverScope(t.Context(), []int64{group}), "", "", 1, 20)
	require.ErrorIs(t, err, service.ErrObserverScope)
}
