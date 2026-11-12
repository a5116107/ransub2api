//go:build integration

package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type priorityConfigStore interface {
	service.SettingRepository
	CompareAndSetPrioritySchedulingConfig(context.Context, *string, string) (bool, error)
}

func newPriorityConfigRepository(t *testing.T) priorityConfigStore {
	t.Helper()
	_, client := repository.UpstreamChannelsTestDatabase(t)
	repo, ok := repository.NewSettingRepository(client).(priorityConfigStore)
	require.True(t, ok, "the SQL repository must support conditional configuration writes")
	return repo
}

// Pause after the old client has read SQL, not before: the other instance
// commits a real change while that request still holds its stale snapshot.
type priorityConfigReadBarrier struct {
	priorityConfigStore
	read, resume chan struct{}
	paused       atomic.Bool
}

func (r *priorityConfigReadBarrier) GetValue(ctx context.Context, key string) (string, error) {
	value, err := r.priorityConfigStore.GetValue(ctx, key)
	if key == "priority_scheduling_v1" && r.paused.CompareAndSwap(false, true) {
		close(r.read)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return value, err
}

func priorityConfigRouter(settings service.SettingRepository) *gin.Engine {
	h := admin.NewSettingHandler(service.NewSettingService(settings, nil), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.PUT("/config", h.SavePriorityScheduling)
	return router
}

func savePriorityConfig(router *gin.Engine, body []byte) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/config", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestPriorityConfigConcurrentLegacySave(t *testing.T) {
	repo := newPriorityConfigRepository(t)
	for _, tc := range []struct {
		field      string
		old, fresh any
	}{
		{"preserve_session_affinity", true, false},
		{"balance_protocols", true, false},
		{"oauth_quota_priority", true, false},
		{"oauth_quota_threshold", 85, 95},
		{"error_rate_weight", 15, 30},
		{"error_rate_window_minutes", 10, 20},
		{"error_rate_prior_samples", 5, 10},
		{"latency_penalty_mode", "deprioritize", "stop"},
		{"latency_penalty_minutes", 10, 20},
		{"latency_percentile", 70, 95},
		{"latency_measurement", "legacy_forward", "attributed_v1"},
		{"recovery_exploration_percent", 2, 5},
	} {
		t.Run(tc.field, func(t *testing.T) {
			raw, err := json.Marshal(service.DefaultPrioritySchedulingConfig())
			require.NoError(t, err)
			var old map[string]any
			require.NoError(t, json.Unmarshal(raw, &old))
			old[tc.field] = tc.old
			raw, err = json.Marshal(old)
			require.NoError(t, err)
			require.NoError(t, repo.Set(t.Context(), "priority_scheduling_v1", string(raw)))
			barrier := &priorityConfigReadBarrier{priorityConfigStore: repo, read: make(chan struct{}), resume: make(chan struct{})}
			var released atomic.Bool
			release := func() {
				if released.CompareAndSwap(false, true) {
					close(barrier.resume)
				}
			}
			defer release()
			legacy, modern := priorityConfigRouter(barrier), priorityConfigRouter(repo)
			delete(old, tc.field)
			old["mode"] = "profit"
			body, err := json.Marshal(old)
			require.NoError(t, err)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- savePriorityConfig(legacy, body) }()
			select {
			case <-barrier.read:
			case <-time.After(5 * time.Second):
				t.Fatal("old client did not read its initial snapshot")
			}
			old[tc.field], old["mode"] = tc.fresh, "experience"
			body, err = json.Marshal(old)
			require.NoError(t, err)
			response := savePriorityConfig(modern, body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			release()
			select {
			case response = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("old client did not finish after the concurrent commit")
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			stored, err := repo.GetValue(t.Context(), "priority_scheduling_v1")
			require.NoError(t, err)
			var final, expected map[string]any
			require.NoError(t, json.Unmarshal([]byte(stored), &final))
			expectedRaw, err := json.Marshal(tc.fresh)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal([]byte(`{"value":`+string(expectedRaw)+`}`), &expected))
			require.Equal(t, expected["value"], final[tc.field], "an omitted field must retain the concurrently committed value")
			require.Equal(t, "profit", final["mode"], "the old client's explicit update must still be applied")
			var returned struct{ Data map[string]any }
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &returned))
			require.Equal(t, final, returned.Data, "the response must describe the configuration actually committed")
		})
	}
}

func TestPriorityConfigConcurrentCreate(t *testing.T) {
	repo := newPriorityConfigRepository(t)
	barrier := &priorityConfigReadBarrier{priorityConfigStore: repo, read: make(chan struct{}), resume: make(chan struct{})}
	legacy, modern := priorityConfigRouter(barrier), priorityConfigRouter(repo)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- savePriorityConfig(legacy, []byte(`{"enabled":true,"mode":"profit"}`)) }()
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(barrier.resume)
		}
	}
	defer release()
	select {
	case <-barrier.read:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy request did not observe the missing row")
	}
	want := service.DefaultPrioritySchedulingConfig()
	want.PreserveSessionAffinity, want.BalanceProtocols, want.OAuthQuotaPriority = true, false, true
	want.ErrorRateWeight, want.ErrorRateWindowMinutes, want.ErrorRatePriorSamples = 35, 20, 10
	want.OAuthQuotaThreshold = 85
	want.LatencyPenaltyMode, want.LatencyPenaltyMinutes = "stop", 20
	want.LatencyPercentile, want.LatencyMeasurement, want.RecoveryExplorationPercent = 95, "attributed_v1", 5
	body, err := json.Marshal(want)
	require.NoError(t, err)
	response := savePriorityConfig(modern, body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	release()
	select {
	case response = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy request did not retry after a concurrent insertion")
	}
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	want.Enabled, want.Mode = true, "profit"
	stored, err := service.NewSettingService(repo, nil).GetPrioritySchedulingConfig(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, stored)
	var returned struct {
		Data service.PrioritySchedulingConfig
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &returned))
	require.Equal(t, stored, returned.Data)
}

type priorityConfigWriteHook struct {
	priorityConfigStore
	beforeWrite func(context.Context, int32) error
	calls       atomic.Int32
}

func (r *priorityConfigWriteHook) CompareAndSetPrioritySchedulingConfig(ctx context.Context, previous *string, value string) (bool, error) {
	call := r.calls.Add(1)
	if r.beforeWrite != nil {
		if err := r.beforeWrite(ctx, call); err != nil {
			return false, err
		}
	}
	return r.priorityConfigStore.CompareAndSetPrioritySchedulingConfig(ctx, previous, value)
}

func TestPriorityConfigSaveFailureAndRecovery(t *testing.T) {
	repo := newPriorityConfigRepository(t)
	for _, tc := range []struct {
		name        string
		status      int
		failedCalls int32
	}{
		{"conflict", http.StatusConflict, 3},
		{"write_error", http.StatusServiceUnavailable, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			latest := service.DefaultPrioritySchedulingConfig()
			raw, err := json.Marshal(latest)
			require.NoError(t, err)
			require.NoError(t, repo.Set(t.Context(), "priority_scheduling_v1", string(raw)))
			hook := &priorityConfigWriteHook{priorityConfigStore: repo}
			hook.beforeWrite = func(ctx context.Context, call int32) error {
				if tc.name == "write_error" {
					return errors.New("private database failure")
				}
				latest.ErrorRateWindowMinutes++
				raw, err := json.Marshal(latest)
				if err != nil {
					return err
				}
				return repo.Set(ctx, "priority_scheduling_v1", string(raw))
			}
			router := priorityConfigRouter(hook)
			body := []byte(`{"enabled":true,"mode":"profit"}`)
			response := savePriorityConfig(router, body)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Equal(t, tc.failedCalls, hook.calls.Load())
			require.NotContains(t, response.Body.String(), "private database failure")
			if tc.name == "conflict" {
				require.Contains(t, response.Body.String(), "PRIORITY_SCHEDULING_CONFIG_CONFLICT")
			}
			stored, err := service.NewSettingService(repo, nil).GetPrioritySchedulingConfig(t.Context())
			require.NoError(t, err)
			require.Equal(t, latest, stored, "a failed save must not commit stale or explicit request fields")
			hook.beforeWrite = nil
			response = savePriorityConfig(router, body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Equal(t, tc.failedCalls+1, hook.calls.Load())
			latest.Enabled, latest.Mode = true, "profit"
			stored, err = service.NewSettingService(repo, nil).GetPrioritySchedulingConfig(t.Context())
			require.NoError(t, err)
			require.Equal(t, latest, stored)
			var returned struct {
				Data service.PrioritySchedulingConfig
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &returned))
			require.Equal(t, stored, returned.Data)
		})
	}
}

func TestPriorityConfigConditionalStorage(t *testing.T) {
	repo := newPriorityConfigRepository(t)
	for _, tc := range []struct {
		name, initial, previous        string
		exists, absent, stored, cancel bool
	}{
		{name: "create_absent", absent: true, stored: true},
		{name: "create_existing", exists: true, initial: "old", absent: true},
		{name: "replace_exact", exists: true, initial: "old", previous: "old", stored: true},
		{name: "reject_stale", exists: true, initial: "newer", previous: "old"},
		{name: "deleted_not_recreated", previous: "old"},
		{name: "empty_is_existing", exists: true, absent: true},
		{name: "empty_exact", exists: true, stored: true},
		{name: "corrupt_is_existing", exists: true, initial: "{", absent: true},
		{name: "canceled_create", absent: true, cancel: true},
		{name: "canceled_replace", exists: true, initial: "old", previous: "old", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, repo.Delete(t.Context(), "priority_scheduling_v1"))
			if tc.exists {
				require.NoError(t, repo.Set(t.Context(), "priority_scheduling_v1", tc.initial))
			}
			previous := &tc.previous
			if tc.absent {
				previous = nil
			}
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			stored, err := repo.CompareAndSetPrioritySchedulingConfig(ctx, previous, "replacement")
			if tc.cancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.stored, stored)
			actual, err := repo.GetValue(t.Context(), "priority_scheduling_v1")
			switch {
			case tc.stored:
				require.NoError(t, err)
				require.Equal(t, "replacement", actual)
			case tc.exists:
				require.NoError(t, err)
				require.Equal(t, tc.initial, actual)
			default:
				require.ErrorIs(t, err, service.ErrSettingNotFound)
			}
		})
	}
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprintf("one_winner_exists_%t", exists), func(t *testing.T) {
			require.NoError(t, repo.Delete(t.Context(), "priority_scheduling_v1"))
			var previous *string
			if exists {
				old := "old"
				previous = &old
				require.NoError(t, repo.Set(t.Context(), "priority_scheduling_v1", old))
			}
			type outcome struct {
				value  string
				stored bool
				err    error
			}
			start, done := make(chan struct{}), make(chan outcome, 8)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for i := 0; i < 8; i++ {
				go func() {
					<-start
					value := fmt.Sprint(i)
					stored, err := repo.CompareAndSetPrioritySchedulingConfig(ctx, previous, value)
					done <- outcome{value, stored, err}
				}()
			}
			close(start)
			winners, value := 0, ""
			for i := 0; i < 8; i++ {
				result := <-done
				require.NoError(t, result.err)
				if result.stored {
					winners++
					value = result.value
				}
			}
			require.Equal(t, 1, winners)
			stored, err := repo.GetValue(t.Context(), "priority_scheduling_v1")
			require.NoError(t, err)
			require.Equal(t, value, stored)
		})
	}
}
