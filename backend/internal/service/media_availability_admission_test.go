//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSeedanceAndMiniMaxAvailabilityFailClosedBeforeSending(t *testing.T) {
	reader := &availabilityAdmissionRepo{reports: []*AvailabilityReport{{
		AccountID:  9,
		Configured: true,
		Enabled:    true,
	}}}

	t.Run("seedance", func(t *testing.T) {
		upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task-1"}`)}
		svc := &OpenAIGatewayService{httpUpstream: upstream, availabilityReader: reader}
		c, _ := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)

		_, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointCreate, "", []byte(`{"model":"video","content":[{"type":"text","text":"waves"}]}`))

		var denied *OpenAITurnAdmissionError
		require.ErrorAs(t, err, &denied)
		require.Equal(t, "availability_scope_unconfirmed", denied.Reason)
		require.Empty(t, upstream.requests)
	})

	t.Run("minimax_h3", func(t *testing.T) {
		upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task-1"}`)}
		svc := &OpenAIGatewayService{httpUpstream: upstream, availabilityReader: reader}
		c, _ := grokMediaContentTestContext(http.MethodPost, "/v1/videos", nil)

		_, err := svc.ForwardMiniMaxH3(context.Background(), c, miniMaxH3TestAccount(), MiniMaxH3EndpointCreate, "", []byte(`{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`))

		var denied *OpenAITurnAdmissionError
		require.ErrorAs(t, err, &denied)
		require.Equal(t, "availability_scope_unconfirmed", denied.Reason)
		require.Empty(t, upstream.requests)
	})
}
