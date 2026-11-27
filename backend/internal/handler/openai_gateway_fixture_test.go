package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Register immediately after construction, including when a fixture later
// replaces a handler's gateway. Capture the instance, not the mutable field.
func ownOpenAIGatewayFixture(t testing.TB, gateway *service.OpenAIGatewayService) {
	t.Helper()
	t.Cleanup(func() {
		gateway.StopAsyncVideoKeyReleaseRecovery()
		gateway.StopBPS403Recovery()
		gateway.StopBPSWarmPool()
		gateway.StopOpenAICodexTicketHarvester()
		gateway.CloseOpenAIWSPool()
		require.NoError(t, gateway.CloseExcelBPSImages())
	})
}
