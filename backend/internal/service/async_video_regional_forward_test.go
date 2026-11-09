//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type asyncVideoRegionalForwardStub struct {
	HTTPUpstream
	AsyncVideoKeyLeaseController
	dispatch video.Dispatch
	forward  func(context.Context, video.Query) (*http.Response, bool, error)
	owner    *AsyncVideoKeyOwner
}

func TestAsyncVideoRegionalForwardPreservesSecurityClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, source := range []string{"session-binding", "custom-header", "verified-forward"} {
		t.Run(source, func(t *testing.T) {
			stub := &asyncVideoRegionalForwardStub{forward: func(_ context.Context, query video.Query) (*http.Response, bool, error) {
				require.Equal(t, "198.51.100.42", query.ClientIP, "origin ACL must use the same security address as ingress authentication")
				return nil, false, nil
			}}
			svc := &OpenAIGatewayService{httpUpstream: stub}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/task", nil)
			c.Request.Header.Set("Authorization", "Bearer downstream-fixture")
			switch source {
			case "session-binding":
				c.Request = c.Request.WithContext(WithSessionBinding(c.Request.Context(), &SessionBinding{IP: "198.51.100.42"}))
			case "custom-header":
				ip.SetForwardedIPSettings(c, true, []string{"X-Client-IP"})
				c.Request.Header.Set("X-Client-IP", "198.51.100.42")
			case "verified-forward":
				ip.SetVerifiedForwardedIP(c, "198.51.100.42")
			}
			handled, err := svc.ForwardRegionalVideoQuery(c, GrokMediaEndpointVideoStatus, "task", &APIKey{ID: 2}, 1)
			require.NoError(t, err)
			require.False(t, handled)
		})
	}
}

func (s *asyncVideoRegionalForwardStub) SetAsyncVideoForwardHandler(dispatch video.Dispatch) {
	s.dispatch = dispatch
}
func (s *asyncVideoRegionalForwardStub) ForwardAsyncVideoLookup(ctx context.Context, q video.Query) (*http.Response, bool, error) {
	return s.forward(ctx, q)
}
func (s *asyncVideoRegionalForwardStub) ResolveAsyncVideoKeyOwner(context.Context, AsyncVideoKeyOwner, string) (*AsyncVideoKeyOwner, error) {
	return s.owner, nil
}

func TestAsyncVideoRegionalForwardDispatchUsesOriginalRouter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		endpoint           GrokMediaEndpoint
		task, path, method string
	}{
		{GrokMediaEndpointVideoStatus, "task", "/v1/videos/task", http.MethodGet},
		{GrokMediaEndpointVideoContent, "task", "/v1/videos/task/content", http.MethodGet},
		{SeedanceEndpointStatus, "seedance:task", "/v1/contents/generations/tasks/task", http.MethodGet},
		{SeedanceEndpointDelete, "seedance:task", "/v1/contents/generations/tasks/task", http.MethodDelete},
		{MiniMaxH3EndpointStatus, "minimax-h3:task", "/v1/videos/task", http.MethodGet},
		{MiniMaxH3EndpointContent, "minimax-h3:task", "/v1/videos/task/content", http.MethodGet},
	} {
		t.Run(string(tc.endpoint), func(t *testing.T) {
			owner := AsyncVideoKeyOwner{UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Provider: tc.endpoint.AsyncVideoProvider(), RouteFingerprint: [32]byte{1}}
			key := &APIKey{ID: 2, GroupID: &owner.GroupID}
			stub := &asyncVideoRegionalForwardStub{owner: &owner, forward: func(context.Context, video.Query) (*http.Response, bool, error) {
				t.Error("a received query must never forward again")
				return nil, true, ErrUpstreamKeyPolicy
			}}
			svc := &OpenAIGatewayService{httpUpstream: stub}
			router := gin.New()
			calls := 0
			router.Handle(tc.method, tc.path, func(c *gin.Context) {
				calls++
				require.Equal(t, "Bearer downstream-fixture", c.GetHeader("Authorization"))
				require.Equal(t, "192.0.2.5:0", c.Request.RemoteAddr)
				require.Empty(t, c.GetHeader("Cookie"))
				require.Empty(t, c.GetHeader("X-Forwarded-For"))
				require.Equal(t, "bytes=0-99", c.GetHeader("Range"))
				handled, err := svc.ForwardRegionalVideoQuery(c, tc.endpoint, tc.task, key, 1)
				require.NoError(t, err)
				require.False(t, handled)
				account, err := svc.ResolveMediaVideoRequestAccount(c.Request.Context(), key.GroupID, tc.task, 1, key.ID, tc.endpoint)
				require.NoError(t, err)
				require.Equal(t, owner.AccountID, account)
				wrong := *key
				wrong.ID++
				handled, err = svc.ForwardRegionalVideoQuery(c, tc.endpoint, tc.task, &wrong, 1)
				require.True(t, handled)
				require.ErrorIs(t, err, ErrUpstreamKeyPolicy, "receiver authentication cannot change the original principal")
				changed := owner
				changed.RouteFingerprint[0]++
				stub.owner = &changed
				_, err = svc.ResolveMediaVideoRequestAccount(c.Request.Context(), key.GroupID, tc.task, 1, key.ID, tc.endpoint)
				require.ErrorIs(t, err, ErrUpstreamKeyPolicy, "changed local ownership must be rejected after the network wait")
				c.String(http.StatusOK, "original-router")
			})
			svc.InstallMediaVideoForwarding(router)
			require.NotNil(t, stub.dispatch)
			query := video.Query{Owner: owner, TaskID: tc.task, Operation: string(tc.endpoint), Credential: "downstream-fixture", ClientIP: "192.0.2.5", Range: "bytes=0-99"}
			response := httptest.NewRecorder()
			stub.dispatch(response, httptest.NewRequest(http.MethodPost, "/private", nil), query)
			require.Equal(t, 1, calls)
			require.Equal(t, "original-router", response.Body.String())
			for _, task := range []string{"../create", ".", "..", "task?api_key=bad"} {
				query.TaskID = task
				response := httptest.NewRecorder()
				stub.dispatch(response, httptest.NewRequest(http.MethodPost, "/private", nil), query)
				require.Equal(t, http.StatusForbidden, response.Code)
			}
			require.Equal(t, 1, calls)
		})
	}
}

func TestAsyncVideoRegionalForwardStreamsWithoutLocalExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	owner := AsyncVideoKeyOwner{UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Provider: "grok", RouteFingerprint: [32]byte{1}}
	key := &APIKey{ID: owner.APIKeyID, GroupID: &owner.GroupID}
	for _, mode := range []string{"remote", "local", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			stub := &asyncVideoRegionalForwardStub{forward: func(_ context.Context, q video.Query) (*http.Response, bool, error) {
				calls++
				require.Equal(t, owner.UserID, q.Owner.UserID)
				require.Zero(t, q.Owner.AccountID, "the receiving node must not guess an upstream account")
				require.Equal(t, "downstream-fixture", q.Credential)
				if mode == "local" {
					return nil, false, nil
				}
				if mode == "unavailable" {
					return nil, true, ErrUpstreamKeyPolicy
				}
				return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": {"video/mp4"}, "Set-Cookie": {"private=value"}, "X-Regional-Video-Authority": {video.PrimaryAuthority}}, Body: io.NopCloser(strings.NewReader("video-data"))}, true, nil
			}}
			svc := &OpenAIGatewayService{httpUpstream: stub}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/task/content", nil)
			c.Request.Header.Set("Authorization", "Bearer downstream-fixture")
			handled, err := svc.ForwardRegionalVideoQuery(c, GrokMediaEndpointVideoContent, "task", key, owner.UserID)
			require.Equal(t, mode != "local", handled)
			require.Equal(t, 1, calls)
			if mode == "unavailable" {
				require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
				require.False(t, c.Writer.Written())
				return
			}
			require.NoError(t, err)
			if mode == "remote" {
				require.Equal(t, http.StatusPartialContent, recorder.Code)
				require.Equal(t, "video-data", recorder.Body.String())
				require.Empty(t, recorder.Header().Get("Set-Cookie"))
				require.Empty(t, recorder.Header().Get("X-Regional-Video-Authority"))
			}
		})
	}
}

func TestAsyncVideoRegionalForwardPreservesMiniMaxContentRedirect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, location := range []string{"https://video.example/output.mp4", "http://video.example/output.mp4", "https://user:pass@video.example/output.mp4", ""} {
		t.Run(location, func(t *testing.T) {
			stub := &asyncVideoRegionalForwardStub{forward: func(context.Context, video.Query) (*http.Response, bool, error) {
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader(""))}, true, nil
			}}
			svc := &OpenAIGatewayService{httpUpstream: stub}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/task/content", nil)
			c.Request.Header.Set("Authorization", "Bearer downstream-fixture")
			handled, err := svc.ForwardRegionalVideoQuery(c, MiniMaxH3EndpointContent, "minimax-h3:task", &APIKey{ID: 2}, 1)
			require.True(t, handled)
			if location != "https://video.example/output.mp4" {
				require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
				require.False(t, c.Writer.Written())
				return
			}
			require.NoError(t, err)
			require.Equal(t, http.StatusFound, recorder.Code)
			require.Equal(t, location, recorder.Header().Get("Location"))
		})
	}
}
