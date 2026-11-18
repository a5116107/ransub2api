package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// Invalid HTTP header name, like duplicate-operation metadata. Repository
// adapters remove it before exposing ExtraHeaders to any service or handler.
const ChannelMonitorBudgetCredentialScopeMetadataKey = "sub2api:budget_credential_scope"

const channelMonitorBudgetScopePublishTimeout = 2 * time.Second

// Only generated inside the original plaintext/encryption boundary. This is
// evidence of credential identity, not capability, health, freshness or quota.
type ChannelMonitorBudgetCredentialScope struct {
	Version int      `json:"version"`
	Source  string   `json:"source"`
	Keys    []string `json:"keys"`
}

func monitorCredentialFingerprints(adapter providerAdapter, apiKey string, opts *CheckOptions) []string {
	identity := &http.Request{Header: make(http.Header)}
	for key, value := range mergeHeaders(adapter.buildHeaders(apiKey), opts) {
		identity.Header.Set(key, value)
	}
	return config.UpstreamCredentialFingerprints(identity)
}

func channelMonitorBudgetCredentialSource(m *ChannelMonitor) string {
	if m == nil || validateAPIMode(m.Provider, m.APIMode) != nil {
		return ""
	}
	adapter, mode, ok := providerAdapterFor(m.Provider, m.APIMode)
	if !ok {
		return ""
	}
	headers := make(map[string]string, len(m.ExtraHeaders))
	for key, value := range m.ExtraHeaders {
		if key != ChannelMonitorBudgetCredentialScopeMetadataKey && key != ChannelMonitorDuplicateOperationIDMetadataKey {
			headers[key] = value
		}
	}
	// Bind both the opaque ciphertext and the current adapter/header contract.
	// Template changes invalidate old evidence without decrypting in a preview.
	raw, err := json.Marshal(struct {
		Version    int
		Provider   string
		APIMode    string
		CheckMode  string
		Ciphertext string
		Headers    map[string]string
		Template   map[string]string
	}{1, m.Provider, mode, defaultCheckMode(m.CheckMode), m.APIKey, headers, mergeHeaders(adapter.buildHeaders("__monitoring_scope_template__"), &CheckOptions{ExtraHeaders: headers})})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (m *ChannelMonitor) setBudgetCredentialScope(plain string) {
	m.BudgetCredentialScope = nil
	if defaultCheckMode(m.CheckMode) == MonitorCheckModeQuota || m.APIKeyDecryptFailed {
		return
	}
	source := channelMonitorBudgetCredentialSource(m)
	if source == "" {
		return
	}
	adapter, _, _ := providerAdapterFor(m.Provider, m.APIMode)
	keys := monitorCredentialFingerprints(adapter, plain, &CheckOptions{ExtraHeaders: m.ExtraHeaders})
	slices.Sort(keys)
	scope := &ChannelMonitorBudgetCredentialScope{Version: 1, Source: source, Keys: keys}
	m.BudgetCredentialScope = scope
	if len(ValidatedChannelMonitorBudgetCredentials(m)) == 0 {
		m.BudgetCredentialScope = nil
	}
}

// Input APIKey must be the persisted ciphertext, not the decrypted service
// response. Returned copies cannot mutate a later compiler observation.
func ValidatedChannelMonitorBudgetCredentials(m *ChannelMonitor) []string {
	if m == nil || m.BudgetCredentialScope == nil {
		return nil
	}
	s := m.BudgetCredentialScope
	if s.Version != 1 || s.Source == "" || s.Source != channelMonitorBudgetCredentialSource(m) || len(s.Keys) < 1 || len(s.Keys) > 4 {
		return nil
	}
	for i, key := range s.Keys {
		if len(key) != 64 || (i > 0 && key <= s.Keys[i-1]) {
			return nil
		}
		decoded, err := hex.DecodeString(key)
		if err != nil || hex.EncodeToString(decoded) != key {
			return nil
		}
	}
	return slices.Clone(s.Keys)
}

// Startup and RunCheck already decrypt these rows. Publish identity evidence
// only there, never on GET/list/preview. Late or failed publication is neutral
// to business/probe quality and cannot overwrite a newer config (repository CAS).
func (s *ChannelMonitorService) decryptAndPublishBudgetScope(ctx context.Context, m *ChannelMonitor) {
	if m == nil {
		return
	}
	stored := *m
	s.decryptInPlace(m)
	stored.APIKeyDecryptFailed = m.APIKeyDecryptFailed
	stored.setBudgetCredentialScope(m.APIKey)
	before, after := m.BudgetCredentialScope, stored.BudgetCredentialScope
	if before == nil && after == nil || before != nil && after != nil && before.Version == after.Version && before.Source == after.Source && slices.Equal(before.Keys, after.Keys) {
		return
	}
	if context.Cause(ctx) != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, channelMonitorBudgetScopePublishTimeout)
	defer cancel()
	updated, err := s.repo.RecordBudgetCredentialScope(writeCtx, &stored)
	if err != nil {
		// Do not log storage errors that could echo ciphertext or header values.
		slog.Warn("channel_monitor: budget credential evidence publication failed", "monitor_id", m.ID)
		return
	}
	if updated {
		m.BudgetCredentialScope = after
	}
}
