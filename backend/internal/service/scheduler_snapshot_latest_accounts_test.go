//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type latestSnapshotIDCache struct{ *retryAccountCache }

func (c *latestSnapshotIDCache) SetSnapshotAndReturnAccountIDs(ctx context.Context, b SchedulerBucket, token SchedulerBucketWriteToken, accounts []Account) ([]int64, error) {
	if err := c.SetSnapshot(ctx, b, token, accounts); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return ids, nil
}
func (c *latestSnapshotIDCache) SetSnapshotByAccountIDs(context.Context, SchedulerBucket, SchedulerBucketWriteToken, []int64) error {
	return nil
}

func TestSchedulerSnapshotUsesLatestCompleteAccounts(t *testing.T) {
	for _, mode := range []string{"snapshot", "snapshot_ids", "snapshot_ids_fallback"} {
		t.Run(mode, func(t *testing.T) {
			old := Account{ID: 101, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"access_token": "synthetic-old"}, Extra: map[string]any{"openai_excel_bps": true}}
			current := old
			current.Status = StatusDisabled
			current.Schedulable = false
			current.ErrorMessage = "synthetic quality disable"
			current.Credentials = map[string]any{"access_token": "synthetic-new"}
			current.Extra = map[string]any{"openai_excel_bps": false, "codex_5h_used_percent": 100.0, CodexUsageObservationVersionKey: "200"}
			second := Account{ID: 103, Platform: PlatformOpenAI}
			source := &retryAccountRepository{current: []Account{second, current}}
			cache := &retryAccountCache{}
			var underlying SchedulerCache = cache
			if mode == "snapshot_ids" {
				underlying = &latestSnapshotIDCache{cache}
			}
			wrapped := &codexOrderedSchedulerCache{SchedulerCache: underlying, source: source}
			bucket := SchedulerBucket{Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
			token := SchedulerBucketWriteToken{Bucket: bucket, Epoch: 1}
			input := []Account{old, {ID: 102}, second}
			if mode == "snapshot" {
				require.NoError(t, wrapped.SetSnapshot(context.Background(), bucket, token, input))
			} else {
				ids, err := wrapped.SetSnapshotAndReturnAccountIDs(context.Background(), bucket, token, input)
				require.NoError(t, err)
				require.Equal(t, []int64{101, 103}, ids)
			}
			require.Equal(t, []Account{current, second}, cache.snapshot, "full rebuild must publish current complete fields, preserve input order and omit deleted IDs")
		})
	}
}

func TestSchedulerSnapshotIDRetryFiltersLatestMembership(t *testing.T) {
	old := Account{ID: 101, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, GroupIDs: []int64{10}}
	for _, state := range []string{"moved", "disabled", "deleted"} {
		t.Run(state, func(t *testing.T) {
			current := old
			source := &retryAccountRepository{current: []Account{current}}
			switch state {
			case "moved":
				source.current[0].GroupIDs = []int64{20}
			case "disabled":
				source.current[0].Schedulable = false
			case "deleted":
				source.current = nil
			}
			cache := &retryAccountCache{}
			wrapped := &codexOrderedSchedulerCache{SchedulerCache: &latestSnapshotIDCache{cache}, source: source}
			bucket := SchedulerBucket{GroupID: 10, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
			ctx := context.WithValue(context.Background(), schedulerOutboxRetryPublicationKey{}, true)
			ids, err := wrapped.SetSnapshotAndReturnAccountIDs(ctx, bucket, SchedulerBucketWriteToken{Bucket: bucket, Epoch: 1}, []Account{old})
			require.NoError(t, err)
			require.Empty(t, ids)
			require.Empty(t, cache.snapshot)
		})
	}
}

func TestSchedulerSnapshotRetryRetainsCoolingMembers(t *testing.T) {
	until := time.Now().Add(time.Minute)
	for _, kind := range []string{"temporary", "overload", "rate_limit"} {
		for _, idWriter := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ids_%t", kind, idWriter), func(t *testing.T) {
				account := Account{ID: 101, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, GroupIDs: []int64{10}}
				switch kind {
				case "temporary":
					account.TempUnschedulableUntil = &until
				case "overload":
					account.OverloadUntil = &until
				case "rate_limit":
					account.RateLimitResetAt = &until
				}
				source := &retryAccountRepository{current: []Account{account}}
				cache := &retryAccountCache{}
				wrapped := &codexOrderedSchedulerCache{SchedulerCache: &latestSnapshotIDCache{cache}, source: source}
				bucket := SchedulerBucket{GroupID: 10, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
				ctx := context.WithValue(t.Context(), schedulerOutboxRetryPublicationKey{}, true)
				token := SchedulerBucketWriteToken{Bucket: bucket, Epoch: 1}
				if idWriter {
					ids, err := wrapped.SetSnapshotAndReturnAccountIDs(ctx, bucket, token, []Account{account})
					require.NoError(t, err)
					require.Equal(t, []int64{account.ID}, ids)
				} else {
					require.NoError(t, wrapped.SetSnapshot(ctx, bucket, token, []Account{account}))
				}
				require.Len(t, cache.snapshot, 1)
				require.False(t, cache.snapshot[0].IsSchedulable(), "retaining membership must not bypass cooldown")
			})
		}
	}
}
