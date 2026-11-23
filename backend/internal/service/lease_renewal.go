package service

import (
	"context"
	"sync"
	"time"
)

// The watchdog is independent of renewal I/O and measures validity from the
// start of the last acknowledged operation. Callers cancel on renewal
// errors and reserve one third of the TTL for their transport to join.
func keepRenewedLease(ctx context.Context, acquiredAt time.Time, ttl time.Duration, renew func(context.Context) (bool, error), onLoss func()) func() {
	validFor := ttl * 2 / 3
	workerCtx, cancelWorker := context.WithCancel(ctx)
	done := make(chan struct{})
	var mu sync.Mutex
	stopped := false
	validUntil := acquiredAt.Add(validFor)
	lose := func() {
		mu.Lock()
		defer mu.Unlock()
		if !stopped {
			stopped = true
			onLoss()
			cancelWorker()
		}
	}
	watchdog := time.AfterFunc(time.Until(validUntil), lose)
	go func() {
		defer close(done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				if workerCtx.Err() != nil {
					return
				}
				started := time.Now()
				opCtx, cancel := context.WithTimeout(workerCtx, 2*time.Second)
				owned, err := renew(opCtx)
				cancel()
				if err != nil || !owned {
					lose()
					return
				}
				mu.Lock()
				now := time.Now()
				late := !now.Before(validUntil) || !now.Before(started.Add(validFor))
				if !stopped && !late && workerCtx.Err() == nil {
					validUntil = started.Add(validFor)
					watchdog.Reset(time.Until(validUntil))
				}
				mu.Unlock()
				if late {
					lose()
					return
				}
			}
		}
	}()
	return sync.OnceFunc(func() {
		mu.Lock()
		stopped = true
		watchdog.Stop()
		mu.Unlock()
		cancelWorker()
		<-done
	})
}
