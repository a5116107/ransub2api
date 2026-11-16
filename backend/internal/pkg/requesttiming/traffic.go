package requesttiming

import "context"

type trafficKey struct{}

// TrafficSourceObservation records a server-owned entry point, not the intent
// of an arbitrary API caller. Gateway traffic may include undeclared probes.
type TrafficSourceObservation struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	Kind    string `json:"kind"`
}

const (
	TrafficGateway     = "gateway_ingress"
	TrafficAccountTest = "account_connection_test"
)

// WithTrafficSource is used only by trusted server call sites. No request
// header, session name, prompt, account setting or billing row supplies it.
func WithTrafficSource(ctx context.Context, source string) context.Context {
	observation := TrafficSourceObservation{Version: 1, Source: source}
	switch source {
	case TrafficGateway:
		observation.Kind = "gateway"
	case TrafficAccountTest:
		observation.Kind = "synthetic"
	default:
		observation = TrafficSourceObservation{}
	}
	return context.WithValue(ctx, trafficKey{}, observation)
}

func inheritTrafficSource(ctx context.Context, collector *Collector) {
	if ctx == nil || collector == nil {
		return
	}
	observation, ok := ctx.Value(trafficKey{}).(TrafficSourceObservation)
	if !ok || observation.Version != 1 {
		return
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if !collector.finished && collector.data.TrafficSource == nil {
		collector.data.TrafficSource = &observation
	}
}
