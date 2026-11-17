package repository

import (
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/upstreamstats"
	"github.com/Wei-Shaw/sub2api/internal/requestcapture"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Shared physical HTTP boundary, including TLS and regional egress. Ordinary
// traffic remains passive; internally marked billing probes acquire a budget.
type accountResponseTransport struct{ base http.RoundTripper }

func (t *accountResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req, finish, err := service.BeginMonitoringProbeRequest(req)
	if err != nil {
		if req != nil && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if requesttiming.From(req.Context()) != nil {
		requesttiming.RecordUpstreamCredentials(req.Context(), config.UpstreamCredentialFingerprints(req))
		req = requesttiming.ObserveUpstreamRequest(req)
	}
	resp, err := t.base.RoundTrip(req)
	err = service.FinishMonitoringProbeResponse(req.Context(), resp, err, finish)
	if err == nil && resp != nil && req.Context().Err() == nil && isAccountInferenceRequest(req) {
		upstreamstats.Default.Record(requestcapture.AccountFromContext(req.Context()), resp.StatusCode, time.Now())
	}
	return resp, err
}

func isAccountInferenceRequest(req *http.Request) bool {
	if req.Method != http.MethodPost || req.URL == nil {
		return false
	}
	path := strings.TrimRight(req.URL.Path, "/")
	for _, suffix := range []string{"/messages", "/responses", "/responses/compact", "/chat/completions", "/completions", ":generateContent", ":streamGenerateContent"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
