package service

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func isModerationKeyAdmissionFailure(err error) bool {
	return IsUpstreamKeyAdmissionFailure(err) || errors.Is(err, ErrUpstreamKeyLease)
}

func (s *ContentModerationService) setUpstreamKeyAuthority(cfg *config.Config, upstream HTTPUpstream) {
	if s == nil {
		return
	}
	s.requireUpstreamKeyAdmission = upstreamKeyAdmissionRequired(cfg)
	s.upstreamKeyAdmission, _ = upstream.(UpstreamKeyRequestAdmission)
}

type moderationAdmissionTransport struct {
	base      http.RoundTripper
	authority UpstreamKeyRequestAdmission
	mu        sync.Mutex
	boundCtx  context.Context
	localErr  error
}

func (t *moderationAdmissionTransport) recordLocalError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case errors.Is(err, ErrUpstreamKeyCapacity):
		t.localErr = ErrUpstreamKeyCapacity
	case errors.Is(err, ErrUpstreamKeyLease):
		t.localErr = ErrUpstreamKeyLease
	default:
		t.localErr = ErrUpstreamKeyPolicy
	}
}

func (t *moderationAdmissionTransport) localError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.boundCtx != nil && errors.Is(context.Cause(t.boundCtx), ErrUpstreamKeyLease) {
		return ErrUpstreamKeyLease
	}
	return t.localErr
}

func (t *moderationAdmissionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	bound, finish, err := t.authority.BeginUpstreamKeyRequest(req)
	if err != nil {
		if finish != nil {
			finish(nil, err)
		}
		t.recordLocalError(err)
		return nil, err
	}
	if bound == nil || finish == nil {
		if finish != nil {
			finish(nil, ErrUpstreamKeyPolicy)
		}
		t.recordLocalError(ErrUpstreamKeyPolicy)
		return nil, ErrUpstreamKeyPolicy
	}
	t.mu.Lock()
	t.boundCtx = bound.Context()
	t.mu.Unlock()
	resp, err := t.base.RoundTrip(bound)
	finish(resp, err)
	if errors.Is(context.Cause(bound.Context()), ErrUpstreamKeyLease) {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.recordLocalError(ErrUpstreamKeyLease)
		return nil, ErrUpstreamKeyLease
	}
	return resp, err
}

func (s *ContentModerationService) admitModerationClient(client *http.Client) (*http.Client, error) {
	if !s.requireUpstreamKeyAdmission {
		return client, nil
	}
	if client == nil || s.upstreamKeyAdmission == nil {
		return nil, ErrUpstreamKeyPolicy
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy := *client
	copy.Transport = &moderationAdmissionTransport{base: base, authority: s.upstreamKeyAdmission}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy, nil
}

func moderationLocalAdmissionError(client *http.Client) error {
	if client == nil {
		return nil
	}
	if transport, ok := client.Transport.(*moderationAdmissionTransport); ok {
		return transport.localError()
	}
	return nil
}
