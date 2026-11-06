package service

import (
	"context"
	"errors"
	"time"
)

var ErrOpenAIResponseOwnerUnavailable = errors.New("response owner unavailable")

// OpenAIResponseOwner binds tenant authorization and creation egress in one
// record. An account row can change without changing its ID.
type OpenAIResponseOwner struct {
	UserID   int64                     `json:"user_id"`
	APIKeyID int64                     `json:"api_key_id"`
	Scope    OpenAIWSContinuationScope `json:"scope"`
}

func (o OpenAIResponseOwner) Valid() bool {
	return o.UserID > 0 && o.APIKeyID > 0 && o.Scope.valid()
}

// OpenAIResponseOwnerCache is an optional, authoritative shared-owner
// adapter. Production Redis implements it without expanding GatewayCache and
// breaking unrelated adapters. Responses continuation requires this capability when
// a shared cache is configured; legacy integer pairs cannot prove a route.
type OpenAIResponseOwnerCache interface {
	SetResponseOwner(ctx context.Context, groupID int64, responseID string, owner OpenAIResponseOwner, ttl time.Duration) error
	GetResponseOwner(ctx context.Context, groupID int64, responseID string) (owner OpenAIResponseOwner, found bool, err error)
}
