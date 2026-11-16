package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type connectionRecorderTest struct{ records []service.ConnectionTrace }

func TestRequestTimingRetainsServerTrafficSource(t *testing.T) {
	for _, method := range []string{"POST", "GET"} {
		t.Run(method, func(t *testing.T) {
			recorder := &connectionRecorderTest{}
			router := gin.New()
			router.Use(RequestTiming(recorder))
			router.Handle(method, "/v1/responses", func(c *gin.Context) {
				if method == "GET" {
					turn := requesttiming.NewWSTurn(time.Now(), 11, 0, [32]byte{})
					requesttiming.ObserveIndependentTurn(c.Request.Context(), turn.Collector)
					turn.Finish(nil, false, false)
				}
				c.Status(200)
			})
			request := httptest.NewRequest(method, "/v1/responses", strings.NewReader("{}"))
			request.Header.Set("X-Sub2API-Traffic-Source", "account_connection_test")
			request.Header.Set("X-Session-Id", "cache-test:untrusted-name")
			router.ServeHTTP(httptest.NewRecorder(), request)
			require.Len(t, recorder.records, 1)
			encoded, err := json.Marshal(recorder.records[0].Detail)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(encoded, &got))
			require.Equal(t, map[string]any{"version": float64(1), "source": "gateway_ingress", "kind": "gateway"}, got["traffic_source"])
		})
	}
}

func (r *connectionRecorderTest) RecordConnectionTrace(record service.ConnectionTrace) {
	r.records = append(r.records, record)
}

func TestRequestTimingOtherRoutesRequireAnExplicitObservation(t *testing.T) {
	recorder := &connectionRecorderTest{}
	router := gin.New()
	router.Use(RequestTiming(recorder))
	router.GET("/health", func(c *gin.Context) { c.Status(200) })
	router.POST("/api/v1/admin/accounts/:id/test", func(c *gin.Context) {
		collector := requesttiming.New(time.Now(), 0)
		ctx := requesttiming.WithTrafficSource(c.Request.Context(), requesttiming.TrafficAccountTest)
		ctx = requesttiming.With(ctx, collector)
		requesttiming.ObserveIndependentTurn(ctx, collector)
		collector.Finish(200, false)
		c.Status(200)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	require.Empty(t, recorder.records, "ordinary non-gateway routes must not create diagnostics")
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/admin/accounts/11/test", nil))
	require.Len(t, recorder.records, 1)
	require.Equal(t, requesttiming.TrafficAccountTest, recorder.records[0].Detail.TrafficSource.Source)
	require.Equal(t, "synthetic", recorder.records[0].Detail.TrafficSource.Kind)
	require.Zero(t, recorder.records[0].APIKeyID, "operator tests do not claim a downstream key or sale group")
	require.Zero(t, recorder.records[0].GroupID)
}

func TestConnectionMetadataWithoutBillingOrAuthentication(t *testing.T) {
	recorder := &connectionRecorderTest{}
	r := gin.New()
	r.Use(RequestLogger(), RequestTiming(recorder))
	r.POST("/v1/responses", func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	request := httptest.NewRequest("POST", "http://relay.example.com:443/v1/responses?api_key=not-recorded", strings.NewReader("private body"))
	request.RemoteAddr = "192.0.2.1:32000"
	request.Header.Set("X-Forwarded-Proto", "https") // Untrusted peer must not set the scheme.
	r.ServeHTTP(httptest.NewRecorder(), request)
	require.Len(t, recorder.records, 1)
	got := recorder.records[0]
	require.Zero(t, got.UserID)
	require.Equal(t, 401, got.Detail.Status)
	require.Equal(t, "relay.example.com:443", got.Host)
	require.Equal(t, "http", got.Scheme)
	require.Equal(t, "/v1/responses", got.Path)
	require.NotEmpty(t, got.RequestID)
	require.NotEmpty(t, got.TraceID)
	require.Equal(t, "application", got.Source)
}

func TestFailedSSEConnectionRecordedWithNoUsage(t *testing.T) {
	recorder := &connectionRecorderTest{}
	r := gin.New()
	r.Use(RequestLogger(), RequestTiming(recorder))
	r.POST("/v1/responses", func(c *gin.Context) {
		key := &service.APIKey{ID: 7, UserID: 9}
		c.Set(string(ContextKeyAPIKey), key)
		c.Set("ops_model", "gpt-test")
		c.Set("ops_stream", true)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, "client-one"))
		_, _ = io.ReadAll(c.Request.Body)
		c.Writer.WriteHeader(200)
		_, _ = c.Writer.Write([]byte("event: response.failed\ndata: {}\n\n"))
		requesttiming.Outcome(c.Request.Context(), "failed", false)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}")))
	require.Len(t, recorder.records, 1)
	got := recorder.records[0]
	require.Equal(t, int64(9), got.UserID)
	require.Equal(t, int64(7), got.APIKeyID)
	require.Equal(t, 200, got.Detail.Status)
	require.Equal(t, "failed", got.Detail.Outcome)
	require.True(t, got.Detail.BodyComplete)
	require.Equal(t, "client-one", got.ClientRequestID)
}

func TestIndependentWSTurnsRecordedWithoutUsageAndFreezeIdentity(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			recorder := &connectionRecorderTest{}
			r := gin.New()
			r.Use(RequestLogger(), RequestTiming(recorder))
			r.Handle(method, "/v1/responses", func(c *gin.Context) {
				group := int64(13)
				c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: 7, UserID: 9, GroupID: &group})
				c.Set("ops_model", "initial-alias-is-not-per-turn-evidence")
				for range 2 {
					turn := requesttiming.NewWSTurn(time.Now(), 11, 0, [32]byte{})
					requesttiming.ObserveIndependentTurn(c.Request.Context(), turn.Collector)
					// Mutation after registration cannot change the frozen identity.
					c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 10})
					turn.Finish(context.DeadlineExceeded, false, false)
					c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: 7, UserID: 9, GroupID: &group})
				}
			})
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/v1/responses", strings.NewReader("{}")))
			want := 2
			if method == "POST" {
				want++
			}
			require.Len(t, recorder.records, want)
			require.NotEqual(t, recorder.records[0].TraceID, recorder.records[1].TraceID)
			for _, got := range recorder.records[:2] {
				require.EqualValues(t, 7, got.APIKeyID)
				require.EqualValues(t, 9, got.UserID)
				require.EqualValues(t, 13, got.GroupID)
				require.Equal(t, "failed", got.Detail.Outcome)
				require.Equal(t, "ws_turn", got.Detail.TTFTMode)
				require.Empty(t, got.Model)
				require.True(t, got.Stream)
			}
		})
	}
}
