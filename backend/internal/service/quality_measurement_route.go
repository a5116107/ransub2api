package service

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// QualityRouteEvidence is internal forwarding evidence. Only the repository's
// private result envelope persists it; neither rule JSON nor API results accept
// or expose account/connection fingerprints. It attests to the selected account
// configuration, not the provider's internal route or physical network exit.
type QualityRouteEvidence struct {
	Version            int       `json:"version"`
	AccountID          int64     `json:"account_id"`
	Fingerprint        string    `json:"fingerprint"`
	ConnectionID       string    `json:"connection_id,omitempty"`
	ConnectionRevision int64     `json:"connection_revision,omitempty"`
	MeasurementHash    string    `json:"measurement_hash"`
	ObservedAt         time.Time `json:"observed_at"`
}

type QualityMeasurementRouteValidity struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// MatchesResult binds the private observation to this frozen sample, including
// its original upstream finish time rather than a later grading completion.
func (e *QualityRouteEvidence) MatchesResult(result *ScheduledTestResult) bool {
	if e == nil || e.Version != 1 || e.AccountID <= 0 || result == nil || result.PelicanConfig == nil {
		return false
	}
	m := result.PelicanConfig.QualityMeasurement
	probe := qualityMeasurementProbe(result)
	if m == nil || m.Scope == nil || probe == nil || probe.Scope == nil {
		return false
	}
	measuredScope, observedScope := *m.Scope, *probe.Scope
	measuredScope.ConnectionScope, observedScope.ConnectionScope = "", ""
	fingerprint, err := hex.DecodeString(e.Fingerprint)
	measurementHash, hashErr := hex.DecodeString(e.MeasurementHash)
	return err == nil && len(fingerprint) == 32 && len(e.Fingerprint) == 64 && e.Fingerprint == strings.ToLower(e.Fingerprint) &&
		hashErr == nil && len(measurementHash) == 32 && m.Version == qualityMeasurementVersion && len(m.MeasurementHash) == 64 && measuredScope == observedScope &&
		!m.CompletedAt.Before(m.ObservedAt) && qualityMeasurementSQLTime(m.CompletedAt).Equal(qualityMeasurementSQLTime(result.FinishedAt)) &&
		m.MeasurementHash == e.MeasurementHash && !e.ObservedAt.IsZero() && m.ObservedAt.Equal(e.ObservedAt) &&
		e.ObservedAt.Equal(probe.FinishedAt) &&
		((e.ConnectionID == "" && e.ConnectionRevision == 0) || (e.ConnectionID != "" && e.ConnectionRevision > 0))
}

// Capture before the send. An exact returned Request/body association is still
// required by the existing observer before this candidate becomes evidence.
// OAuth/parent/shadow identities need additional chain proof and stay unknown.
func qualityRouteForRequest(account *Account, request *http.Request, proxy string) *QualityRouteEvidence {
	if account == nil || account.ID <= 0 || !account.IsOpenAI() || account.Type != AccountTypeAPIKey || account.ParentAccountID != nil ||
		request == nil || request.URL == nil || request.URL.User != nil || request.URL.Fragment != "" || request.URL.RawQuery != "" ||
		(request.Host != "" && !strings.EqualFold(request.Host, request.URL.Host)) ||
		(account.ProxyID != nil && account.Proxy == nil) {
		return nil
	}
	expectedProxy := ""
	if account.Proxy != nil {
		expectedProxy = account.Proxy.URL()
	}
	if proxy != expectedProxy {
		return nil
	}
	identity, valid := availabilityRequestIdentity(request)
	key := account.GetCredential("api_key")
	if !valid || key == "" || identity[0] != "Bearer "+key {
		return nil
	}
	for _, value := range identity[1:] {
		if value != "" {
			return nil // No proof that an extra identity header belongs to this key.
		}
	}
	address := *request.URL
	switch address.Scheme {
	case "ws":
		address.Scheme = "http"
	case "wss":
		address.Scheme = "https"
	case "http", "https":
	default:
		return nil
	}
	endpoint := "/v1/responses"
	if strings.HasSuffix(address.Path, "/chat/completions") {
		endpoint = "/v1/chat/completions"
	}
	expected, err := url.Parse(buildOpenAIEndpointURL(account.GetOpenAIBaseURL(), endpoint))
	if err != nil || expected.User != nil || expected.Fragment != "" || expected.String() != address.String() {
		return nil
	}
	fingerprint, valid := AvailabilityPreparedRouteFingerprint(account)
	if !valid {
		return nil
	}
	return &QualityRouteEvidence{Version: 1, AccountID: account.ID, Fingerprint: hex.EncodeToString(fingerprint[:]),
		ConnectionID: account.AuthConnectionID, ConnectionRevision: account.AuthConnectionRevision}
}
