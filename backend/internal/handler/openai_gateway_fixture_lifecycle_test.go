//go:build unit

package handler

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestResponsesFailoverFixtureReleasesBackgroundWorkers(t *testing.T) {
	const rounds = 8
	gateways := make([]weak.Pointer[service.OpenAIGatewayService], 0, rounds)
	// A failing reproduction still owns its workers until this test exits.
	t.Cleanup(func() {
		for _, pointer := range gateways {
			if gateway := pointer.Value(); gateway != nil {
				gateway.StopOpenAICodexTicketHarvester()
				gateway.CloseOpenAIWSPool()
			}
		}
	})
	for range rounds {
		t.Run("fixture", func(t *testing.T) {
			handler := newOpenAIResponsesFailoverTestHandler(t, &openAIResponsesFailoverCancelUpstream{})
			gateways = append(gateways, weak.Make(handler.gatewayService))
		})
	}
	remaining := func() int {
		runtime.GC()
		count := 0
		for _, pointer := range gateways {
			if pointer.Value() != nil {
				count++
			}
		}
		return count
	}
	require.Eventually(t, func() bool { return remaining() == 0 }, 3*time.Second, 20*time.Millisecond,
		"completed fixture subtests must release their gateway workers, not retain them until process exit")
}

func TestServiceTierFixtureReleasesBillingWorkers(t *testing.T) {
	var pointer weak.Pointer[service.BillingCacheService]
	t.Cleanup(func() {
		if billing := pointer.Value(); billing != nil {
			billing.Stop()
		}
	})
	t.Run("fixture", func(t *testing.T) {
		handler := newServiceTierHandlerTest(t)
		pointer = weak.Make(handler.billingCacheService)
	})
	require.Eventually(t, func() bool {
		runtime.GC()
		return pointer.Value() == nil
	}, 3*time.Second, 20*time.Millisecond, "completed fixture must close its billing workers")
}
