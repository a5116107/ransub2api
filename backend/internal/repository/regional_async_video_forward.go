package repository

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (c *regionalConcurrencyCache) prepareVideoForwarding() error {
	if c.runtimeConfig == nil || c.runtimeConfig.VideoForward == nil {
		return nil
	}
	if !c.hasAsyncVideoDirectory() {
		return video.ErrUnavailable
	}
	peer := video.PrimaryAuthority
	if c.primary != nil {
		peer = video.LocalAuthority(c.runtimeConfig.Scope.Region)
	}
	forwarder, err := video.NewForwarder(*c.runtimeConfig.VideoForward, c.persistentUpstreamKeyAuthority(), peer, c.resolveLocalVideoForwardOwner)
	if err != nil {
		return err
	}
	c.videoForwarder = forwarder
	return nil
}

// Only this SQL authority can prove a location. No peer lookup or supplier
// request is performed while answering another node's locate/query request.
func (c *regionalConcurrencyCache) resolveLocalVideoForwardOwner(ctx context.Context, principal video.Owner, taskID string) (*video.Owner, error) {
	record, err := c.readRegionalAsyncVideoTask(ctx, video.TaskKey(principal, taskID), true)
	if err != nil {
		return nil, err
	}
	if record == nil || record.Fenced || record.TaskID != taskID || !video.SamePrincipal(principal, record.Owner) {
		return nil, service.ErrUpstreamKeyPolicy
	}
	return &record.Owner, nil
}

func (s *httpUpstreamService) SetAsyncVideoForwardHandler(dispatch video.Dispatch) {
	if s != nil && s.upstreamKeyLimiter != nil && s.upstreamKeyLimiter.regional != nil && s.upstreamKeyLimiter.regional.videoForwarder != nil {
		s.upstreamKeyLimiter.regional.videoForwarder.SetDispatch(dispatch)
	}
}

func (s *httpUpstreamService) ForwardAsyncVideoLookup(ctx context.Context, query video.Query) (*http.Response, bool, error) {
	if s == nil || s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.regional == nil || s.upstreamKeyLimiter.regional.runtimeConfig == nil || s.upstreamKeyLimiter.regional.runtimeConfig.VideoForward == nil {
		return nil, false, nil
	}
	store, err := s.regionalAsyncVideoStore()
	if err != nil || store.videoForwarder == nil {
		return nil, true, service.ErrUpstreamKeyPolicy
	}
	owner, err := s.ResolveAsyncVideoKeyOwner(ctx, query.Owner, query.TaskID)
	if err == nil && owner != nil {
		return nil, false, nil
	}
	if !errors.Is(err, video.ErrTaskNotLocal) && !errors.Is(err, video.ErrTaskUnknown) {
		return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, err)
	}
	// A sole assigned peer may prove a retained terminal receipt even after its
	// active directory entry disappeared. Absence is never a sticky/account guess.
	remoteOwner, err := store.videoForwarder.Locate(ctx, query.Owner, query.TaskID)
	if err != nil {
		return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, err)
	}
	query.Owner = *remoteOwner
	response, err := store.videoForwarder.Query(ctx, query)
	if err != nil {
		return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, err)
	}
	return response, true, nil
}
