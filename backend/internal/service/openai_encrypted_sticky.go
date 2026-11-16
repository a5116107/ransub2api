package service

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
)

type openAIEncryptedReasoningAffinityKey struct{}

type openAIEncryptedSessionBindingKey struct{}

// WithOpenAIEncryptedSessionBinding keeps HTTP history affinity provisional
// until the complete response succeeds. The original observation is shared by
// selection and slot admission so a rejected replacement cannot pin the next
// request, nor can a slow completion overwrite a newer successful binding.
func (s *OpenAIGatewayService) WithOpenAIEncryptedSessionBinding(ctx context.Context, groupID *int64, sessionHash string) context.Context {
	if openAIEncryptedSessionBindingDeferred(ctx, groupID, sessionHash) {
		return ctx
	}
	if ctx.Value(openAIEncryptedReasoningAffinityKey{}) != true || strings.TrimSpace(sessionHash) == "" || s == nil || s.cache == nil {
		return ctx
	}
	accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash)
	ctx = context.WithValue(ctx, openAIEncryptedSessionBindingKey{}, true)
	return withOpenAIStickyBindingSnapshot(ctx, groupID, sessionHash, accountID, err == nil || errors.Is(err, ErrStickySessionNotFound))
}

func openAIEncryptedSessionBindingDeferred(ctx context.Context, groupID *int64, sessionHash string) bool {
	return ctx.Value(openAIEncryptedSessionBindingKey{}) == true && openAIStickyBindingSnapshotFromContext(ctx, groupID, sessionHash) != nil
}

// BindOpenAIEncryptedSessionAfterSuccess is only called after a complete HTTP
// response, never on admission, partial output, rejection or cancellation.
// Profit-control and compare-and-swap rules remain the binding authority.
func (s *OpenAIGatewayService) BindOpenAIEncryptedSessionAfterSuccess(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if !openAIEncryptedSessionBindingDeferred(ctx, groupID, sessionHash) || ctx.Err() != nil {
		return nil
	}
	return s.bindOpenAIStickySessionAfterAdmission(ctx, groupID, sessionHash, accountID)
}

// WithOpenAIEncryptedReasoningAffinity marks a Responses admission once, before
// account selection. It does not change hard admission or capacity decisions.
func WithOpenAIEncryptedReasoningAffinity(ctx context.Context, body []byte) context.Context {
	// Escaped keys may still contain the field, so retain the original lookup.
	if !bytes.Contains(body, []byte("encrypted_content")) && !bytes.Contains(body, []byte(`\u`)) {
		return context.WithValue(ctx, openAIEncryptedReasoningAffinityKey{}, false)
	}
	input := gjson.GetBytes(body, "input")
	found := false
	visit := func(item gjson.Result) {
		if item.Get("type").String() == "reasoning" {
			e := item.Get("encrypted_content")
			found = found || e.Type == gjson.String && e.String() != ""
		}
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool { visit(item); return !found })
	} else if input.IsObject() {
		visit(input)
	}
	return context.WithValue(ctx, openAIEncryptedReasoningAffinityKey{}, found)
}
