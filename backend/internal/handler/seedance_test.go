//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type retryableSeedanceCache struct {
	*grokMediaSlotBindings
	released atomic.Int32
}

func (s *retryableSeedanceCache) ReleaseGrokVideoBilled(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(s.billed, key)
	s.released.Add(1)
	return nil
}

type cancelOnceSeedanceBilling struct {
	service.UsageBillingRepository
	mu       sync.Mutex
	attempts int
}

func (b *cancelOnceSeedanceBilling) Apply(context.Context, *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	b.mu.Lock()
	b.attempts++
	attempt := b.attempts
	b.mu.Unlock()
	if attempt > 1 {
		return &service.UsageBillingApplyResult{Applied: true}, nil
	}
	// Billing has its own detached context and can outlast the worker deadline.
	time.Sleep(50 * time.Millisecond)
	return nil, context.DeadlineExceeded
}

func TestSeedanceHandlerLifecycleAndOwnership(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	var owner int64
	upstream.call = func(req *http.Request, id int64) (*http.Response, error) {
		body := `{"id":"task-ark","status":"queued"}`
		if req.Method == http.MethodPost {
			owner = id
		} else {
			require.Equal(t, owner, id)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	newContext := func(method string) (*gin.Context, *httptest.ResponseRecorder) {
		c, w := grokMediaSlotContext(context.Background(), method == http.MethodPost)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.Platform = service.PlatformOpenAI
		body := ""
		if method == http.MethodPost {
			body = `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`
		}
		c.Request = httptest.NewRequest(method, "/api/v3/contents/generations/tasks", strings.NewReader(body))
		c.Params = gin.Params{{Key: "task_id", Value: "task-ark"}}
		return c, w
	}
	c, w := newContext(http.MethodPost)
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Positive(t, owner)
	require.Len(t, bindings.pending, 1)
	slots.assertReleased(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		c, w = newContext(method)
		h.SeedanceTasks(c)
		require.Equal(t, 200, w.Code, w.Body.String())
		slots.assertReleased(t)
	}
	for _, other := range []string{"user", "key", "group", "task", "provider"} {
		c, w = newContext(http.MethodGet)
		key, _ := middleware.GetAPIKeyFromContext(c)
		switch other {
		case "user":
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 5})
		case "key":
			key.ID = 21
		case "group":
			group := int64(25)
			key.GroupID = &group
		case "task":
			c.Params = gin.Params{{Key: "task_id", Value: "other"}}
		case "provider":
			c.Params = gin.Params{{Key: "request_id", Value: "task-ark"}}
		}
		before := upstream.calls
		if other == "provider" {
			h.GrokVideoStatus(c)
		} else {
			h.SeedanceTasks(c)
		}
		require.Equal(t, 404, w.Code, other+": "+w.Body.String())
		require.Equal(t, before, upstream.calls)
		slots.assertReleased(t)
	}
	c, _ = newContext(http.MethodGet)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	result := &service.OpenAIForwardResult{Usage: service.OpenAIUsage{OutputTokens: 12345}, ResponseID: "seedance:task-ark"}
	for i := range 20 {
		billed := prepareSeedanceCompletionBilling(context.Background(), h, key, subject, result.ResponseID, result)
		if i == 0 {
			require.NotNil(t, billed)
			require.Equal(t, "doubao-seedance", billed.BillingModel)
			require.Equal(t, 12345, billed.Usage.OutputTokens)
			require.Zero(t, billed.VideoCount)
		} else {
			require.Nil(t, billed)
		}
	}
	require.Len(t, bindings.billed, 1)
}

func TestSeedanceClaimedTokenBillingSurvivesFullUsageQueue(t *testing.T) {
	h, _, _, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second, OverflowPolicy: "drop",
	})
	h.usageRecordWorkerPool = pool
	t.Cleanup(pool.Stop)
	started, unblock := make(chan struct{}), make(chan struct{})
	defer close(unblock)
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-unblock
	}))
	<-started
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {}))
	c, _ := grokMediaSlotContext(t.Context(), false)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	taskID := service.SeedanceTaskKey("completed")
	pending := service.GrokVideoPendingBilling{Model: "doubao-seedance", BillingModel: "doubao-seedance"}
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(t.Context(), taskID, subject.UserID, key.ID, pending))
	result := prepareSeedanceCompletionBilling(t.Context(), h, key, subject, taskID, &service.OpenAIForwardResult{
		ResponseID: taskID, Usage: service.OpenAIUsage{OutputTokens: 12345},
	})
	require.NotNil(t, result)
	require.Zero(t, result.VideoCount, "Seedance uses token pricing rather than per-video pricing")
	var called atomic.Int32
	h.submitOpenAIUsageRecordTask(t.Context(), result, func(context.Context) { called.Add(1) })
	require.EqualValues(t, 1, called.Load(), "an already-claimed Seedance bill must not be dropped on overflow")
	require.Nil(t, prepareSeedanceCompletionBilling(t.Context(), h, key, subject, taskID, result), "the stable task claim remains once-only")
}

func TestSeedanceCompletionRecordsTokenChargeWithFullUsageQueue(t *testing.T) {
	h, _, bindings, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second, OverflowPolicy: "drop",
	})
	h.usageRecordWorkerPool = pool
	t.Cleanup(pool.Stop)
	started, unblock := make(chan struct{}), make(chan struct{})
	defer close(unblock)
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-unblock
	}))
	<-started
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {}))

	cfg := &config.Config{}
	billing := &compatStreamUsageBillingRepo{}
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	h.gatewayService = service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{
		Config: cfg, Cache: bindings, Billing: service.NewBillingService(cfg, nil),
		UsageBilling: billing, UsageLogs: usage, Deferred: &service.DeferredService{},
	})
	t.Cleanup(h.gatewayService.StopAsyncVideoKeyReleaseRecovery)
	t.Cleanup(h.gatewayService.StopOpenAICodexTicketHarvester)
	c, _ := grokMediaSlotContext(t.Context(), false)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.Group.Platform, key.Group.RateMultiplier = service.PlatformOpenAI, 1
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	taskID := service.SeedanceTaskKey("completed")
	pending := service.GrokVideoPendingBilling{Model: "doubao-seedance", BillingModel: "gpt-5.4"}
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(t.Context(), taskID, subject.UserID, key.ID, pending))
	result := prepareSeedanceCompletionBilling(t.Context(), h, key, subject, taskID, &service.OpenAIForwardResult{
		ResponseID: taskID, Usage: service.OpenAIUsage{OutputTokens: 12345},
	})
	require.NotNil(t, result)
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	recordGrokMediaUsage(c, h, zap.NewNop(), key, subject, nil, account, result, result.Model, nil, taskID)
	require.Len(t, billing.commands, 1, "a full worker queue must still execute the token billing command")
	command := billing.commands[0]
	require.Equal(t, service.StableGrokVideoBillingRequestID(taskID), command.RequestID)
	require.Equal(t, 12345, command.OutputTokens)
	require.Positive(t, command.BalanceCost)
	select {
	case log := <-usage.created:
		require.Equal(t, 12345, log.OutputTokens)
		require.Zero(t, log.VideoCount, "Seedance completion retains token pricing")
	case <-time.After(time.Second):
		t.Fatal("missing token usage log")
	}
	require.Nil(t, prepareSeedanceCompletionBilling(t.Context(), h, key, subject, taskID, result))
	require.Len(t, billing.commands, 1, "repeated status must not bill again")
}

func TestSeedanceBillingFailureAfterCancellationReleasesClaimForRetry(t *testing.T) {
	h, _, bindings, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	cache := &retryableSeedanceCache{grokMediaSlotBindings: bindings}
	billing := &cancelOnceSeedanceBilling{}
	cfg := &config.Config{}
	h.gatewayService = service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{
		Config: cfg, Cache: cache, Billing: service.NewBillingService(cfg, nil),
		UsageBilling: billing, Deferred: &service.DeferredService{},
	})
	t.Cleanup(h.gatewayService.StopAsyncVideoKeyReleaseRecovery)
	t.Cleanup(h.gatewayService.StopOpenAICodexTicketHarvester)
	c, _ := grokMediaSlotContext(t.Context(), false)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.Group.Platform, key.Group.RateMultiplier = service.PlatformOpenAI, 1
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	taskID := service.SeedanceTaskKey("retry")
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(t.Context(), taskID, subject.UserID, key.ID,
		service.GrokVideoPendingBilling{Model: "doubao-seedance", BillingModel: "gpt-5.4"}))
	completed := &service.OpenAIForwardResult{ResponseID: taskID, Usage: service.OpenAIUsage{OutputTokens: 12345}}
	result := prepareSeedanceCompletionBilling(t.Context(), h, key, subject, taskID, completed)
	require.NotNil(t, result)
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1, TaskTimeout: 15 * time.Millisecond, OverflowPolicy: "drop"})
	h.usageRecordWorkerPool = pool
	t.Cleanup(pool.Stop)
	recordGrokMediaUsage(c, h, zap.NewNop(), key, subject, nil, account, result, result.Model, nil, taskID)
	require.Eventually(t, func() bool { return pool.Stats().CompletedTasks == 1 }, 2*time.Second, 5*time.Millisecond)
	require.EqualValues(t, 1, cache.released.Load(), "billing failure must clear the claim even after task cancellation")
	retry := prepareSeedanceCompletionBilling(t.Context(), h, key, subject, taskID, completed)
	require.NotNil(t, retry, "the same task remains eligible for a billing retry")
	recordGrokMediaUsage(c, h, zap.NewNop(), key, subject, nil, account, retry, retry.Model, nil, taskID)
	require.Eventually(t, func() bool { return pool.Stats().CompletedTasks == 2 }, 2*time.Second, 5*time.Millisecond)
	billing.mu.Lock()
	attempts := billing.attempts
	billing.mu.Unlock()
	require.Equal(t, 2, attempts, "the same task retries billing once after the failed attempt")
	require.Nil(t, prepareSeedanceCompletionBilling(t.Context(), h, key, subject, taskID, completed), "successful retry retains the claim")
}
