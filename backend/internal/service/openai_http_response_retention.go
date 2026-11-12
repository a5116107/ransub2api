package service

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
)

// OpenAIHTTPResponseOwnerRetentionStore preserves an existing downstream
// authorization anchor while an upstream account cools down. As with normal
// HTTP continuation validation, a different key of the same user is permitted;
// retention never replaces the original user/key identity.
type OpenAIHTTPResponseOwnerRetentionStore interface {
	RefreshHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, userID, apiKeyID int64, ttl time.Duration) (bool, error)
}

// OpenAIHTTPResponseOwnerRetentionCache compares the exact previously read
// user/key record and its legacy mirrors before extending their deadlines.
// Missing, replaced, or inconsistent records must not be reconstructed.
type OpenAIHTTPResponseOwnerRetentionCache interface {
	RefreshHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, owner OpenAIResponseOwner, ttl time.Duration) (bool, error)
}

func openAIAgentRecoveryHTTPUser(c *gin.Context, keyID int64) int64 {
	if c == nil || keyID <= 0 {
		return 0
	}
	value, _ := c.Get(openAIResponseOwnerContextKey)
	owner, ok := value.(openAIResponseOwner)
	if !ok || owner.apiKeyID != keyID || owner.userID <= 0 {
		return 0
	}
	return owner.userID
}

func (s *defaultOpenAIWSStateStore) RefreshHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, userID, apiKeyID int64, ttl time.Duration) (bool, error) {
	id := normalizeOpenAIWSResponseID(responseID)
	if groupID < 0 || id == "" || userID <= 0 || apiKeyID <= 0 || ttl.Milliseconds() <= 0 {
		return false, errors.New("invalid HTTP response ownership retention")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.cache != nil {
		owners, readable := s.cache.(OpenAIResponseOwnerCache)
		retention, retainable := s.cache.(OpenAIHTTPResponseOwnerRetentionCache)
		if !readable || !retainable {
			return false, nil
		}
		cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
		defer cancel()
		owner, found, err := owners.GetResponseOwner(cacheCtx, groupID, id)
		if err != nil || !found {
			return false, err
		}
		if !owner.Valid() || owner.UserID != userID {
			return false, nil
		}
		return retention.RefreshHTTPResponseOwner(cacheCtx, groupID, id, owner, ttl)
	}

	s.responseOwnerMu.Lock()
	defer s.responseOwnerMu.Unlock()
	mapKey := openAIWSResponseAccountMapKey(groupID, id)
	binding, found := s.responseOwners[mapKey]
	now := time.Now()
	if !found || !now.Before(binding.expiresAt) ||
		!binding.owner.Valid() || binding.owner.UserID != userID {
		return false, nil
	}
	if expiresAt := now.Add(ttl); expiresAt.After(binding.expiresAt) {
		binding.expiresAt = expiresAt
		s.responseOwners[mapKey] = binding
	}
	return true, nil
}

var _ OpenAIHTTPResponseOwnerRetentionStore = (*defaultOpenAIWSStateStore)(nil)
