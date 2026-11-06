package service

import (
	"context"
	"errors"
	"time"
)

var ErrOpenAIHTTPResponseOwnerUnavailable = errors.New("HTTP response owner unavailable")

// OpenAIHTTPResponseOwner binds tenant authorization and creation egress in one
// record. An account row can change without changing its ID.
type OpenAIHTTPResponseOwner struct {
	UserID   int64                     `json:"user_id"`
	APIKeyID int64                     `json:"api_key_id"`
	Scope    OpenAIWSContinuationScope `json:"scope"`
}

func (o OpenAIHTTPResponseOwner) Valid() bool {
	return o.UserID > 0 && o.APIKeyID > 0 && o.Scope.valid()
}

// OpenAIHTTPResponseOwnerCache is an optional, authoritative shared-owner
// adapter. Production Redis implements it without expanding GatewayCache and
// breaking unrelated adapters. HTTP continuation requires this capability when
// a shared cache is configured; legacy integer pairs cannot prove a route.
type OpenAIHTTPResponseOwnerCache interface {
	SetHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, owner OpenAIHTTPResponseOwner, ttl time.Duration) error
	GetHTTPResponseOwner(ctx context.Context, groupID int64, responseID string) (owner OpenAIHTTPResponseOwner, found bool, err error)
}
