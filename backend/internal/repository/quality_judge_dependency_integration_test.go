//go:build integration

package repository_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestQualityJudgeDependencyPostgresDetailAndMembershipIndependence(t *testing.T) {
	db, client := repository.UpstreamChannelsTestDatabase(t)
	ctx := t.Context()
	var accountID, groupID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials)
 VALUES('Diagnostic fixture','openai','apikey','active','{"api_key":"synthetic-private-key","base_url":"https://private.invalid"}') RETURNING id`).Scan(&accountID))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('Grading fixture','openai') RETURNING id`).Scan(&groupID))
	_, err := db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, accountID, groupID)
	require.NoError(t, err)
	plans := repository.NewScheduledTestPlanRepository(db)
	results := repository.NewScheduledTestResultRepository(db)
	cfg := &service.PelicanTestConfig{QuestionKind: "candy", ModelID: "fixture-model", ModelIDs: []string{"fixture-model"},
		ParallelCount: 1, Prompt: "Return 21", Quality: &service.QualityPolicy{Action: service.QualityActionObserveOnly, ExpectedAnswer: "21",
			Judge: &service.QualityJudgeConfig{GroupID: groupID, ModelID: "grader-model", Prompt: "Compare values"}}}
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: accountID, ModelID: cfg.ModelID,
		Enabled: true, CronExpression: "*/5 * * * *", MaxResults: 20, PelicanConfig: cfg})
	require.NoError(t, err)
	users := repository.NewUserRepository(client, db)
	auth := service.NewAuthService(client, users, nil, nil, &config.Config{JWT: config.JWTConfig{Secret: "local-diagnostic-fixture", ExpireHour: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	tokens := map[string]string{}
	for _, role := range []string{"admin", "user", "observer"} {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role) VALUES($1,'fixture',$2) RETURNING id`, "diagnostic-"+role+"@test.invalid", role).Scan(&id))
		user, err := users.GetByID(ctx, id)
		require.NoError(t, err)
		tokens[role], err = auth.GenerateToken(ctx, user)
		require.NoError(t, err)
	}
	h := admin.NewScheduledTestHandler(service.NewScheduledTestService(plans, results))
	router := gin.New()
	protected := router.Group("/api/v1/admin")
	protected.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, service.NewUserService(users, nil, nil, nil), nil, nil)))
	protected.Use((&admin.AccountHandler{}).AuthorizeObserver)
	protected.GET("/scheduled-test-plans/:id/results/:resultID", h.GetResult)
	protected.GET("/scheduled-test-plans/:id/results", h.ListResults)
	protected.DELETE("/scheduled-test-plans/:id", h.Delete)
	protected.PUT("/scheduled-test-plans/:id", h.Update)
	request := func(resultID int64, role string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/admin/scheduled-test-plans/%d/results/%d", plan.ID, resultID), nil)
		if tokens[role] != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[role])
		}
		router.ServeHTTP(w, req)
		return w
	}
	for _, scenario := range []string{"blocked", "responded", "legacy", "unknown_inventory"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			candidates := 2
			judgment := &service.QualityJudgment{Kind: "llm", GroupID: groupID, ModelID: "grader-model", Verdict: "unknown", Reason: "judge_no_available_account",
				Dependency: &service.QualityJudgeDependency{Version: 1, Status: "blocked", Reason: "all_slots_busy", CandidateAccounts: &candidates,
					SlotChecks: 2, BusyAccounts: 2, SelfGradingExcluded: true, RuleRemovesGradingGroup: true, Independence: "not_established"}}
			switch scenario {
			case "responded":
				judgment.Verdict, judgment.Reason = "incorrect", "different values"
				judgment.Dependency.Status, judgment.Dependency.Reason = "responded", "grading_completed"
				judgment.Dependency.SlotChecks, judgment.Dependency.BusyAccounts, judgment.Dependency.RequestCalls = 1, 0, 1
			case "legacy":
				judgment.Dependency = nil
			case "unknown_inventory":
				judgment.Reason, judgment.Dependency.Reason = "judge_group_unavailable", "judge_group_unavailable"
				judgment.Dependency.CandidateAccounts = nil
				judgment.Dependency.SlotChecks, judgment.Dependency.BusyAccounts = 0, 0
			}
			saved, err := results.Create(ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: "success", StartedAt: now, FinishedAt: now,
				ResponseText: "21", QualityAction: "inconclusive", QualityRoundID: now.Format(time.RFC3339Nano), QualityJudgment: judgment, PelicanConfig: cfg})
			require.NoError(t, err)
			require.Equal(t, judgment, saved.QualityJudgment)
			// The actual authenticated handler must preserve scope and frozen data.
			require.Equal(t, 401, request(saved.ID, "").Code)
			require.Equal(t, 403, request(saved.ID, "user").Code)
			require.Equal(t, 404, request(saved.ID, "observer").Code, "out-of-scope result IDs reveal neither existence nor content")
			_, err = db.ExecContext(ctx, `UPDATE users SET observer_group_ids=$1 WHERE email='diagnostic-observer@test.invalid'`, fmt.Sprintf("[%d]", groupID))
			require.NoError(t, err)
			require.Equal(t, 200, request(saved.ID, "observer").Code, "the original assigned observer retains access")
			_, err = db.ExecContext(ctx, `UPDATE users SET observer_group_ids='[]'::jsonb WHERE email='diagnostic-observer@test.invalid'`)
			require.NoError(t, err)
			require.Equal(t, 404, request(saved.ID, "observer").Code, "revoked grants apply to the same JWT on its next read")
			response := request(saved.ID, "admin")
			require.Equal(t, 200, response.Code, response.Body.String())
			var got service.ScheduledTestResult
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
			require.Equal(t, judgment, got.QualityJudgment)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/admin/scheduled-test-plans/%d/results", plan.ID), nil)
			req.Header.Set("Authorization", "Bearer "+tokens["observer"])
			router.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code)
			var hidden []service.ScheduledTestResult
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &hidden))
			require.Empty(t, hidden, "listing by a foreign plan ID must not reveal its historical results")
			require.Equal(t, "inconclusive", got.QualityAction, "diagnostics never grant a restore or admission")
			for _, private := range []string{"synthetic-private-key", "private.invalid", "password_hash", "credentials"} {
				require.NotContains(t, response.Body.String(), private)
			}
			read, err := repository.NewScheduledTestResultRepository(db).ListByPlanID(ctx, plan.ID, 20, false)
			require.NoError(t, err)
			require.Equal(t, judgment, read[0].QualityJudgment)
		})
	}
	denied, granted := service.WithObserverScope(ctx, nil), service.WithObserverScope(ctx, []int64{groupID})
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, fmt.Sprintf("/api/v1/admin/scheduled-test-plans/%d", plan.ID), nil)
		req.Header.Set("Authorization", "Bearer "+tokens["observer"])
		router.ServeHTTP(w, req)
		if method == http.MethodPut {
			require.Equal(t, 404, w.Code)
		} else {
			require.Equal(t, 403, w.Code)
		}
	}
	_, err = plans.GetByID(denied, plan.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	for _, read := range []func() (int, error){
		func() (int, error) { rows, err := plans.ListByAccountID(denied, accountID); return len(rows), err },
		func() (int, error) { rows, err := results.ListByPlanID(denied, plan.ID, 20); return len(rows), err },
	} {
		count, err := read()
		require.NoError(t, err)
		require.Zero(t, count)
	}
	allowed, err := plans.GetByID(granted, plan.ID)
	require.NoError(t, err)
	allowed.MaxResults = 30
	_, err = plans.Update(denied, allowed)
	require.ErrorIs(t, err, service.ErrObserverScope)
	require.ErrorIs(t, plans.Delete(denied, plan.ID), service.ErrObserverScope)
	_, err = plans.Create(denied, &service.ScheduledTestPlan{AccountID: accountID, ModelID: cfg.ModelID, CronExpression: plan.CronExpression, MaxResults: 20, PelicanConfig: cfg})
	require.ErrorIs(t, err, service.ErrObserverScope)
	unchanged, err := plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 20, unchanged.MaxResults, "a denied update rolls back its tentative plan write")
	allowed, err = plans.Update(granted, allowed)
	require.NoError(t, err)
	require.Equal(t, 30, allowed.MaxResults)

	// Hold the same account lock as native group writes, then remove access
	// while an already-started observer plan update waits on that lock.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var locked int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&locked))
	allowed.MaxResults = 40
	finished := make(chan error, 1)
	updateCtx, cancelUpdate := context.WithTimeout(granted, 6*time.Second)
	defer cancelUpdate()
	go func() { _, updateErr := plans.Update(updateCtx, allowed); finished <- updateErr }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE%')`).Scan(&waiting))
		if waiting {
			break
		}
		require.True(t, time.Now().Before(deadline), "observer update must reach its live account-lock wait")
		time.Sleep(20 * time.Millisecond)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1`, accountID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	select {
	case updateErr := <-finished:
		require.ErrorIs(t, updateErr, service.ErrObserverScope, "recheck membership after the blocking native removal")
	case <-time.After(5 * time.Second):
		t.Fatal("observer plan update did not release after membership removal")
	}
	unchanged, err = plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 30, unchanged.MaxResults, "revocation rolls back the blocked update")
	// Product membership is not the inventory of saved detection plans.
	_, err = db.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1`, accountID)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err = db.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, plan.ID, now.Add(-time.Minute))
	require.NoError(t, err)
	due, err := plans.ListDue(ctx, now)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, plan.ID, due[0].ID, "removal must not silently delete or hide the account's detection plan")
	claimed, err := plans.ClaimPelican(ctx, due[0], now, now.Add(time.Minute), now.Add(5*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed, "an account outside product groups can still be claimed for its retest")
	require.NoError(t, plans.FinishPelican(ctx, plan.ID, now.Add(time.Minute), now))
	var enabled bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1`, accountID).Scan(&enabled))
	require.True(t, enabled, "diagnostic reads must preserve manual account state")
}
