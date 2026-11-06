package service

import (
	"context"
	"errors"
	"net/http"
)

// doAdmittedOpenAIClientRequest applies the existing shared-Key response-body
// lease to provider-specific clients that cannot call HTTPUpstream.Do directly.
// The caller owns response parsing and closing; this helper owns admission and
// the redirect boundary.
func doAdmittedOpenAIClientRequest(client *http.Client, req *http.Request, required bool, admission UpstreamKeyRequestAdmission) (*http.Response, error) {
	if client == nil || req == nil {
		return nil, ErrUpstreamKeyPolicy
	}
	var finish func(*http.Response, error)
	if admission != nil {
		var admissionErr error
		req, finish, admissionErr = admission.BeginUpstreamKeyRequest(req)
		if admissionErr != nil {
			if finish != nil {
				finish(nil, admissionErr)
			}
			return nil, admissionErr
		}
		if req == nil || finish == nil {
			if finish != nil {
				finish(nil, ErrUpstreamKeyPolicy)
			}
			return nil, ErrUpstreamKeyPolicy
		}
	} else if required {
		return nil, ErrUpstreamKeyPolicy
	}
	if required {
		copy := *client
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	resp, err := client.Do(req)
	if finish != nil {
		finish(resp, err)
	}
	if err != nil {
		if errors.Is(context.Cause(req.Context()), ErrUpstreamKeyLease) {
			return nil, ErrUpstreamKeyLease
		}
		return nil, err
	}
	if errors.Is(context.Cause(req.Context()), ErrUpstreamKeyLease) {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, ErrUpstreamKeyLease
	}
	return resp, nil
}

func (s *OpenAIGatewayService) doOpenAIClientRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	if s == nil {
		return nil, ErrUpstreamKeyPolicy
	}
	var admission UpstreamKeyRequestAdmission
	if s.httpUpstream != nil {
		admission, _ = s.httpUpstream.(UpstreamKeyRequestAdmission)
	}
	return doAdmittedOpenAIClientRequest(client, req, upstreamKeyAdmissionRequired(s.cfg), admission)
}
