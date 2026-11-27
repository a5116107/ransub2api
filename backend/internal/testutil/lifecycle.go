package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// OwnContentModeration binds worker/queue lifetime to the constructing test,
// including setup failures and services embedded in handler literals.
func OwnContentModeration(t testing.TB, svc *service.ContentModerationService) *service.ContentModerationService {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := svc.Stop(ctx); err != nil {
			t.Errorf("moderation fixture shutdown: %v", err)
		}
	})
	return svc
}
