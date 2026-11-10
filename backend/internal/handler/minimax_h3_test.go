//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMiniMaxH3RejectsAmbiguousPriceBeforeSubmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"seconds", `{"model":"MiniMax-H3","prompt":"waves","seconds":"1","seconds":"15"}`},
		{"escaped_seconds", `{"model":"MiniMax-H3","prompt":"waves","seconds":"1","se\u0063onds":"15"}`},
		{"case_seconds", `{"model":"MiniMax-H3","prompt":"waves","seconds":"1","Seconds":"15"}`},
		{"count", `{"model":"MiniMax-H3","prompt":"waves","seconds":"1","n":1,"n":2}`},
		{"single_case_count", `{"model":"MiniMax-H3","prompt":"waves","seconds":"1","N":2}`},
		{"single_escaped_count", `{"model":"MiniMax-H3","prompt":"waves","seconds":"1","\u006e":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformMiniMax)
			c, w := grokMediaSlotContext(context.Background(), true)
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.Platform = service.PlatformMiniMax
			key.Group.VideoModelPrices = map[string]map[string]float64{service.MiniMaxH3PriceFamily: {service.MiniMaxH3PerSecond: 0.02}}
			key.Group.RateMultiplier = 0.1
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			var observed struct {
				Seconds string `json:"seconds"`
				N       int    `json:"n"`
			}
			upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
				if err := json.NewDecoder(req.Body).Decode(&observed); err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"task","status":"queued"}`))}, nil
			}
			h.MiniMaxH3Videos(c)
			require.Equal(t, http.StatusBadRequest, w.Code, "upstream decoded seconds=%q n=%d", observed.Seconds, observed.N)
			require.Zero(t, upstream.calls, "ambiguous billable fields must fail before provider submission")
			slots.assertReleased(t)
		})
	}
}

func TestMiniMaxH3GroupGateAndLookupOwnership(t *testing.T) {
	for _, scenario := range []string{"disabled", "wrong duration", "accepted", "upstream unavailable", "other key", "other user", "lookup owned", "wrong content type"} {
		t.Run(scenario, func(t *testing.T) {
			h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformMiniMax)
			c, w := grokMediaSlotContext(context.Background(), true)
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.Platform = service.PlatformMiniMax
			key.Group.AllowImageGeneration = false // video has its own switch
			key.Group.VideoModelPrices = map[string]map[string]float64{service.MiniMaxH3PriceFamily: {"request_1s": 0.02}}
			key.Group.RateMultiplier = 0.1
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
				if scenario == "upstream unavailable" {
					return nil, errors.New("network unavailable")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"task","status":"queued"}`))}, nil
			}
			want, calls := http.StatusOK, 1
			switch scenario {
			case "disabled":
				key.Group.VideoModelPrices = nil
				want, calls = http.StatusBadRequest, 0
			case "wrong duration":
				key.Group.VideoModelPrices[service.MiniMaxH3PriceFamily] = map[string]float64{"request_5s": 0.08}
				want, calls = http.StatusBadRequest, 0
			case "wrong content type":
				c.Request.Header.Set("Content-Type", "multipart/form-data")
				want, calls = http.StatusUnsupportedMediaType, 0
			case "upstream unavailable":
				want = http.StatusBadGateway
			case "other key", "other user", "lookup owned":
				require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(context.Background(), key.GroupID, service.MiniMaxH3TaskKey("task"), 10, 20, 1))
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/task", nil)
				if scenario == "other key" {
					key.ID++
				}
				if scenario == "other user" {
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 5})
				}
				if scenario != "lookup owned" {
					want, calls = http.StatusNotFound, 0
				}
			}
			h.MiniMaxH3Videos(c)
			require.Equal(t, want, w.Code, w.Body.String())
			require.Equal(t, calls, upstream.calls, "POST must not retry on ambiguous upstream errors")
			slots.assertReleased(t)
			if scenario == "accepted" {
				pending, err := h.gatewayService.LoadGrokVideoPendingBilling(context.Background(), service.MiniMaxH3TaskKey("task"), 10, 20)
				require.NoError(t, err)
				require.NotNil(t, pending)
				require.InDelta(t, 0.02, *pending.MiniMaxH3BaseCost, 1e-9)
				require.Equal(t, 1, pending.VideoDurationSeconds)
				require.Greater(t, bindings.writes, 0)
			}
		})
	}
}

func TestMiniMaxH3CompletionBillsRequestedSecondsOnce(t *testing.T) {
	h, _, _, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformMiniMax)
	c, _ := grokMediaSlotContext(context.Background(), false)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	taskID := service.MiniMaxH3TaskKey("task")
	base := 0.02
	pending := service.GrokVideoPendingBilling{
		Model: "video-alias", BillingModel: "video-alias", UpstreamModel: "MiniMax-H3",
		VideoResolution: "768p", VideoDurationSeconds: 1, MiniMaxH3BaseCost: &base,
	}
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(context.Background(), taskID, subject.UserID, key.ID, pending))
	loaded, err := h.gatewayService.LoadGrokVideoPendingBilling(context.Background(), taskID, subject.UserID, key.ID)
	require.NoError(t, err)
	require.Equal(t, "768p", loaded.VideoResolution)
	queued := &service.OpenAIForwardResult{ResponseID: taskID}
	require.Nil(t, prepareMiniMaxH3CompletionBilling(context.Background(), h, key, subject, taskID, queued))
	completed := &service.OpenAIForwardResult{ResponseID: taskID, VideoCount: 1, VideoDurationSeconds: 15}
	foreign := subject
	foreign.UserID++
	require.Nil(t, prepareMiniMaxH3CompletionBilling(context.Background(), h, key, foreign, taskID, completed))
	result := prepareMiniMaxH3CompletionBilling(context.Background(), h, key, subject, taskID, completed)
	require.NotNil(t, result)
	require.Equal(t, 1, result.VideoDurationSeconds, "bill requested seconds, not upstream response or MP4 length")
	require.Equal(t, "768p", result.VideoResolution)
	require.Equal(t, base, *result.MiniMaxH3BaseCost)
	require.Equal(t, "video-alias", result.BillingModel)
	require.Nil(t, prepareMiniMaxH3CompletionBilling(context.Background(), h, key, subject, taskID, completed))
	require.NotEqual(t, service.StableGrokVideoBillingRequestID("task"), result.RequestID)
}

func TestMiniMaxH3CreationRetainsSaleMultiplierUntilCompletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate float64
	}{{"paid", 0.1}, {"free", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformMiniMax)
			c, w := grokMediaSlotContext(context.Background(), true)
			key, _ := middleware.GetAPIKeyFromContext(c)
			subject, _ := middleware.GetAuthSubjectFromContext(c)
			key.Group.Platform = service.PlatformMiniMax
			key.Group.VideoModelPrices = map[string]map[string]float64{service.MiniMaxH3PriceFamily: {"request_1s": 0.02}}
			key.Group.RateMultiplier = tc.rate
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			upstream.call = func(*http.Request, int64) (*http.Response, error) {
				// The request has already been quoted when the provider is called.
				key.Group.RateMultiplier = 0.8
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"task","status":"queued"}`))}, nil
			}
			h.MiniMaxH3Videos(c)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			taskID := service.MiniMaxH3TaskKey("task")
			pending, err := h.gatewayService.LoadGrokVideoPendingBilling(context.Background(), taskID, subject.UserID, key.ID)
			require.NoError(t, err)
			require.NotNil(t, pending)
			require.NotNil(t, pending.MiniMaxH3RateMultiplier, "create-time pricing must retain the sale multiplier")
			require.InDelta(t, tc.rate, *pending.MiniMaxH3RateMultiplier, 1e-12)
			completed := &service.OpenAIForwardResult{ResponseID: taskID, VideoCount: 1}
			result := prepareMiniMaxH3CompletionBilling(context.Background(), h, key, subject, taskID, completed)
			require.NotNil(t, result)
			require.NotNil(t, result.MiniMaxH3RateMultiplier)
			require.InDelta(t, tc.rate, *result.MiniMaxH3RateMultiplier, 1e-12, "the later group edit must not reprice the existing task")
		})
	}
}

func TestMiniMaxH3InvalidSnapshotDoesNotClaimAndCanRecover(t *testing.T) {
	h, _, bindings, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformMiniMax)
	c, _ := grokMediaSlotContext(context.Background(), true)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	taskID := service.MiniMaxH3TaskKey("task")
	base, rate := 0.02, 0.1
	pending := service.GrokVideoPendingBilling{Model: "MiniMax-H3", MiniMaxH3BaseCost: &base, MiniMaxH3RateMultiplier: &rate}
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(t.Context(), taskID, subject.UserID, key.ID, pending))
	var cacheKey string
	for k := range bindings.pending {
		cacheKey = k
	}
	require.NotEmpty(t, cacheKey)
	valid := bindings.pending[cacheKey]
	bindings.pending[cacheKey] = []byte(`{"minimax_h3_base_cost":0.02,"minimax_h3_rate_multiplier":null}`)
	completed := &service.OpenAIForwardResult{ResponseID: taskID, VideoCount: 1}
	require.Nil(t, prepareMiniMaxH3CompletionBilling(t.Context(), h, key, subject, taskID, completed))
	require.Empty(t, bindings.billed, "a corrupted snapshot must not consume the billing claim")
	bindings.pending[cacheKey] = valid
	result := prepareMiniMaxH3CompletionBilling(t.Context(), h, key, subject, taskID, completed)
	require.NotNil(t, result)
	require.InDelta(t, rate, *result.MiniMaxH3RateMultiplier, 1e-12)
	require.Nil(t, prepareMiniMaxH3CompletionBilling(t.Context(), h, key, subject, taskID, completed))
}

func TestMiniMaxH3RejectsInvalidSalePriceBeforeSubmission(t *testing.T) {
	for _, tc := range []struct {
		name        string
		price, rate float64
	}{
		{"nan", 0.02, math.NaN()},
		{"positive_infinity", 0.02, math.Inf(1)},
		{"negative_infinity", 0.02, math.Inf(-1)},
		{"overflow", math.MaxFloat64, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformMiniMax)
			c, w := grokMediaSlotContext(context.Background(), true)
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.Platform = service.PlatformMiniMax
			key.Group.VideoModelPrices = map[string]map[string]float64{service.MiniMaxH3PriceFamily: {"request_1s": tc.price}}
			key.Group.RateMultiplier = tc.rate
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			h.MiniMaxH3Videos(c)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Zero(t, upstream.calls)
			slots.assertReleased(t)
		})
	}
}
