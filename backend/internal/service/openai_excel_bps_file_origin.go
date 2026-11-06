package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
)

type excelBPSOriginKey struct{}
type excelBPSOriginFreshReadKey struct{}

type excelBPSOriginGuard struct {
	cache        *basispoints.CatalogCache
	store        basispoints.StateStore
	origin       basispoints.RouteOrigin
	groupID      int64
	enforceGroup bool
}

// The route digest excludes downstream conversation/model, so reopening a
// session on the same actual upstream does not create a false foreign owner.
// Opaque OAuth credentials do not prove a different principal after refresh.
func excelBPSRouteOrigin(account *Account, accountID, token string) basispoints.RouteOrigin {
	route, _ := json.Marshal(struct {
		AccountID    int64
		ConnectionID string
		Fingerprint  [32]byte
	}{account.ID, account.AuthConnectionID, AccountRouteFingerprint(account)})
	origin := basispoints.RouteOrigin{Route: fmt.Sprintf("%x", sha256.Sum256(route))}
	if strings.TrimSpace(accountID) != "" {
		origin.Account = fmt.Sprintf("%x", sha256.Sum256([]byte(accountID)))
	}
	claims, err := openai.DecodeIDToken(token)
	if err == nil && (strings.TrimSpace(claims.Sub) != "" || claims.OpenAIAuth != nil &&
		(strings.TrimSpace(claims.OpenAIAuth.ChatGPTUserID) != "" || strings.TrimSpace(claims.OpenAIAuth.UserID) != "")) {
		origin.Principal = strings.TrimPrefix(excelBPSCacheScope(nil, account, "", "", accountID, token), "bps-v2:")
	}
	return origin
}

func (g *excelBPSOriginGuard) boundedCache(ctx context.Context) (context.Context, *basispoints.CatalogCache, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	if g.store == nil {
		return ctx, g.cache, cancel
	}
	_, cache := basispoints.NewPersistentCaches(ctx, g.store)
	return ctx, cache, cancel
}

func (g *excelBPSOriginGuard) check(ctx context.Context, ids []string) (bool, error) {
	ctx, cache, cancel := g.boundedCache(ctx)
	defer cancel()
	known := false
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		if err := ctx.Err(); err != nil {
			return false, &OpenAITurnAdmissionError{Reason: "bps_file_origin_unavailable", cause: err}
		}
		found, err := cache.CheckFileOrigin(id, g.origin)
		if errors.Is(err, basispoints.ErrRouteOriginMismatch) {
			return true, denyOpenAITurn("bps_file_origin_changed")
		}
		if err != nil {
			return false, &OpenAITurnAdmissionError{Reason: "bps_file_origin_unavailable", cause: err}
		}
		known = known || found
	}
	return known, nil
}

func (g *excelBPSOriginGuard) remember(ctx context.Context, id string) error {
	ctx, cache, cancel := g.boundedCache(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return &OpenAITurnAdmissionError{Reason: "bps_file_origin_unavailable", cause: err}
	}
	err := cache.RememberFileOrigin(id, g.origin)
	if errors.Is(err, basispoints.ErrRouteOriginMismatch) {
		return denyOpenAITurn("bps_file_origin_changed")
	}
	if err != nil {
		return &OpenAITurnAdmissionError{Reason: "bps_file_origin_unavailable", cause: err}
	}
	return nil
}

// encrypted records actual upstream output only; merely passing an unknown
// historical value through must never manufacture ownership evidence.
func (g *excelBPSOriginGuard) encrypted(ctx context.Context, values []string, remember bool) (bool, error) {
	if len(values) == 0 {
		return false, nil
	}
	ctx, cache, cancel := g.boundedCache(ctx)
	defer cancel()
	known := false
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		if err := ctx.Err(); err != nil {
			return false, &OpenAITurnAdmissionError{Reason: "bps_encrypted_origin_unavailable", cause: err}
		}
		var err error
		var found bool
		if remember {
			err = cache.RememberEncryptedOrigin(value, g.origin)
		} else {
			found, err = cache.CheckEncryptedOrigin(value, g.origin)
		}
		if errors.Is(err, basispoints.ErrRouteOriginMismatch) {
			return true, denyOpenAITurn("bps_encrypted_origin_changed")
		}
		if err != nil {
			return false, &OpenAITurnAdmissionError{Reason: "bps_encrypted_origin_unavailable", cause: err}
		}
		known = known || found
	}
	return known, nil
}

// Recheck the concrete references and the current account after proxy waits and
// before each physical attempt. Missing provenance retains compatibility.
func (s *OpenAIGatewayService) guardExcelBPSOriginRequest(req *http.Request, selected *Account) error {
	guard, _ := req.Context().Value(excelBPSOriginKey{}).(*excelBPSOriginGuard)
	if guard == nil {
		return nil
	}
	if req.URL == nil || req.Method != http.MethodPost || req.URL.String() != basispoints.ResponsesURL ||
		(req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host)) {
		return denyOpenAITurn("account_binding_changed")
	}
	body, confirmed := availabilityRequestPayload(req)
	if !confirmed {
		return denyOpenAITurn("bps_file_origin_unavailable")
	}
	ids, err := basispoints.ImageFileReferences(body)
	if err != nil {
		return denyOpenAITurn("bps_file_origin_unavailable")
	}
	ciphertexts, err := basispoints.EncryptedHistoryReferences(body)
	if err != nil {
		return denyOpenAITurn("bps_encrypted_origin_unavailable")
	}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	knownFiles, err := guard.check(ctx, ids)
	if err != nil {
		return err
	}
	knownEncrypted, err := guard.encrypted(ctx, ciphertexts, false)
	if err != nil || (!knownFiles && !knownEncrypted) {
		return err
	}
	ctx = context.WithValue(ctx, excelBPSOriginFreshReadKey{}, true)
	latest, err := s.latestOpenAITurnAccountForGroup(ctx, selected, guard.groupID, guard.enforceGroup)
	if err != nil {
		return err
	}
	if selected.AuthConnectionID != "" {
		if selected.excelBPSParentRoute == nil || *selected.excelBPSParentRoute != excelBPSParentRouteFingerprint(latest) {
			return denyOpenAITurn("account_binding_changed")
		}
	} else if AccountRouteFingerprint(selected) != AccountRouteFingerprint(latest) {
		return denyOpenAITurn("account_binding_changed")
	}
	if _, err = guard.check(ctx, ids); err != nil {
		return err
	}
	_, err = guard.encrypted(ctx, ciphertexts, false)
	return err
}
