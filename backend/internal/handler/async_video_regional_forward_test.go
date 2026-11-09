//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type regionalMediaForwardUpstream struct {
	service.HTTPUpstream
	status int
	calls  int
}

func (*regionalMediaForwardUpstream) SetAsyncVideoForwardHandler(video.Dispatch) {}
func (s *regionalMediaForwardUpstream) ForwardAsyncVideoLookup(_ context.Context, q video.Query) (*http.Response, bool, error) {
	s.calls++
	if s.status == http.StatusServiceUnavailable {
		return nil, true, service.ErrUpstreamKeyPolicy
	}
	return &http.Response{StatusCode: s.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"origin":"fixture"}`))}, true, nil
}

func TestGrokVideoRegionalForwardSkipsReceivingSlots(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false)
			upstream.call = func(*http.Request, int64) (*http.Response, error) {
				t.Error("the receiving node must not contact the provider")
				return nil, service.ErrUpstreamKeyPolicy
			}
			forwarder := &regionalMediaForwardUpstream{HTTPUpstream: upstream, status: status}
			h.gatewayService = service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, bindings, h.cfg, nil, service.NewConcurrencyService(slots), nil, nil, nil, forwarder, nil, nil, nil, nil, nil, nil, nil, nil)
			c, recorder := grokMediaSlotContext(t.Context(), false)
			c.Request.Header.Set("Authorization", "Bearer downstream-fixture")
			h.GrokVideoStatus(c)
			require.Equal(t, status, recorder.Code, recorder.Body.String())
			require.Equal(t, 1, forwarder.calls, "failure must not retry another node or supplier")
			require.Zero(t, slots.userAcquired, "receiving user capacity must not be charged twice")
			require.Zero(t, slots.acquired, "receiving account capacity must not be acquired")
			slots.assertReleased(t)
		})
	}
}
