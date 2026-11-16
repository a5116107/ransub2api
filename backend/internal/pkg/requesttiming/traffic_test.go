package requesttiming

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTrafficSourceInheritanceAndSnapshotIsolation(t *testing.T) {
	for _, source := range []string{TrafficGateway, TrafficAccountTest, "untrusted"} {
		t.Run(source, func(t *testing.T) {
			collector := New(time.Now(), 0)
			ctx := WithTrafficSource(context.Background(), source)
			With(ctx, collector)
			var got Snapshot
			collector.WhenFinished(func(snapshot Snapshot) { got = snapshot })
			collector.Finish(200, false)
			if source == "untrusted" {
				require.Nil(t, got.TrafficSource)
				return
			}
			require.Equal(t, source, got.TrafficSource.Source)
			require.Equal(t, 1, got.TrafficSource.Version)
			got.TrafficSource.Source = "changed"
			collector.WhenFinished(func(snapshot Snapshot) {
				require.Equal(t, source, snapshot.TrafficSource.Source)
			})
		})
	}
}

func TestTrafficSourceIndependentTurnsAndNestedProbe(t *testing.T) {
	parent := New(time.Now(), 0)
	ctx := With(WithTrafficSource(context.Background(), TrafficGateway), parent)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			turn := NewWSTurn(time.Now(), 1, 0, [32]byte{})
			ObserveIndependentTurn(ctx, turn.Collector)
			turn.Finish(nil, false, false)
			turn.WhenFinished(func(snapshot Snapshot) {
				require.Equal(t, TrafficGateway, snapshot.TrafficSource.Source)
			})
		})
	}
	wg.Wait()
	probe := New(time.Now(), 0)
	With(WithTrafficSource(ctx, TrafficAccountTest), probe)
	// An observer or nested binding cannot relabel the probe as gateway traffic.
	ObserveIndependentTurn(ctx, probe)
	probe.Finish(200, false)
	probe.WhenFinished(func(snapshot Snapshot) { require.Equal(t, TrafficAccountTest, snapshot.TrafficSource.Source) })
	parent.Finish(200, false)
	parent.WhenFinished(func(snapshot Snapshot) { require.Equal(t, TrafficGateway, snapshot.TrafficSource.Source) })
	unknown := New(time.Now(), 0)
	unknown.Finish(200, false)
	ObserveIndependentTurn(ctx, unknown)
	unknown.WhenFinished(func(snapshot Snapshot) { require.Nil(t, snapshot.TrafficSource) })
}
