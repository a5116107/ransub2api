package requesttiming

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// WSTurnCapture owns exactly one physical generation, not a socket session.
// It reuses typed Responses content and the existing bounded attempt aggregate;
// HTTP statuses, socket bytes, DNS and handshake timings remain unobserved.
type WSTurnCapture struct {
	*Collector
	ctx   context.Context
	trace *Trace
	once  sync.Once
}

func NewWSTurn(start time.Time, accountID, proxyID int64, fingerprint [32]byte) *WSTurnCapture {
	c := New(start, 0)
	c.data.TTFTMode = "ws_turn"
	c.data.Attempts = append(c.data.Attempts, Attempt{Kind: "ws_turn", Number: 1, AccountID: accountID, ProxyID: proxyID, Events: make(map[string]float64)})
	tr := &Trace{c: c, index: 0}
	ctx := context.WithValue(With(context.Background(), c), attemptKey{}, tr)
	RecordAccountRoute(ctx, fingerprint)
	return &WSTurnCapture{Collector: c, ctx: ctx, trace: tr}
}

// RecordUpstreamCredentials binds a generation to the already established
// socket's verified handshake identity, not current mutable account settings.
func (w *WSTurnCapture) RecordUpstreamCredentials(fingerprints []string) {
	if w != nil {
		RecordUpstreamCredentials(w.ctx, fingerprints)
	}
}

// Content receives the original complete text frame before rewriting. No raw
// frame, response ID, generated text or credential is retained.
func (w *WSTurnCapture) Content(data []byte) {
	if w == nil {
		return
	}
	var event struct {
		Type string `json:"type"`
	}
	if len(data) > 16*1024*1024 || json.Unmarshal(data, &event) != nil {
		Stage(w.ctx, "typed_content_truncated")
		return
	}
	if strings.HasPrefix(event.Type, "content_block_") || strings.HasPrefix(event.Type, "message_") {
		Stage(w.ctx, "typed_content_truncated")
		return
	}
	if event.Type != "error" && !strings.HasPrefix(event.Type, "response.") {
		return
	}
	if !Content(w.ctx, data) {
		Stage(w.ctx, "typed_content_truncated")
	}
}

// DownstreamWrite keeps delivery failures separate from upstream completion.
func (w *WSTurnCapture) DownstreamWrite(err error) {
	if w == nil || err == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.finished {
		w.data.DownstreamError = true
	}
}

func (w *WSTurnCapture) Finish(err error, canceled, disconnected bool) {
	if w == nil {
		return
	}
	w.once.Do(func() {
		outcome := "success"
		w.trace.update(func(a *Attempt) {
			end := w.offset(w.now())
			a.EndMS = &end
			if err != nil {
				a.Error = errorKind(err)
			}
			if err != nil || canceled || w.data.DownstreamError || a.Terminal != "completed" {
				outcome = "failed"
			}
		})
		if disconnected {
			outcome = "client_disconnected"
		}
		Outcome(w.ctx, outcome, disconnected)
		w.Collector.Finish(0, canceled)
	})
}

type usageTurnKey struct{}

// WithUsageTurn preserves independent HTTP diagnostics, but a usage row must
// select only its physical turn, never the enclosing request or socket session.
func WithUsageTurn(ctx context.Context, turn *Collector) context.Context {
	if turn == nil {
		return ctx
	}
	return context.WithValue(ctx, usageTurnKey{}, turn)
}

func UsageCollector(ctx context.Context) *Collector {
	if ctx != nil {
		if c, _ := ctx.Value(usageTurnKey{}).(*Collector); c != nil {
			return c
		}
	}
	return From(ctx)
}
