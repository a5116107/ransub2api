package repository

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (c *regionalConcurrencyCache) prepareVideoForwarding() error {
	if c.runtimeConfig == nil || c.runtimeConfig.VideoForward == nil {
		return nil
	}
	forwarder, err := video.NewForwarder(*c.runtimeConfig.VideoForward, c.persistentUpstreamKeyAuthority(), c.resolveLocalVideoForwardOwner, c.exportAsyncVideoDirectory)
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
	if errors.Is(err, video.ErrTaskNotLocal) || errors.Is(err, video.ErrTaskUnknown) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	if record.Fenced || record.TaskID != taskID || !video.SamePrincipal(principal, record.Owner) {
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
	var remote *video.RemoteTask
	if !errors.As(err, &remote) && !errors.Is(err, video.ErrTaskUnknown) {
		return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, err)
	}
	authority := ""
	var remoteOwner *video.Owner
	if remote != nil {
		// Active tasks select exactly one source from the current SQL view.
		authority = remote.Authority
		remoteOwner, err = store.videoForwarder.Locate(ctx, authority, query.Owner, query.TaskID)
		if err != nil {
			return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, err)
		}
		if remoteOwner == nil || *remoteOwner != remote.Entry.Owner {
			return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, video.ErrConflict)
		}
	} else {
		// Completed receipts are not active-directory entries. Ask only assigned
		// peers for metadata, never credentials or generation. Every peer must
		// prove absence or the same single original source; a timeout is not absence.
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		type location struct {
			authority string
			owner     *video.Owner
			err       error
		}
		candidates := store.videoForwarder.Candidates(query.Owner)
		results := make(chan location, len(candidates))
		for _, peer := range candidates {
			go func() {
				owner, err := store.videoForwarder.Locate(lookup, peer, query.Owner, query.TaskID)
				results <- location{authority: peer, owner: owner, err: err}
			}()
		}
		var lookupErr error
		for range candidates {
			result := <-results
			if errors.Is(result.err, video.ErrTaskUnknown) {
				continue
			}
			if result.err != nil {
				lookupErr = errors.Join(lookupErr, result.err)
			} else if result.owner == nil || remoteOwner != nil {
				lookupErr = errors.Join(lookupErr, video.ErrConflict)
			} else {
				authority, remoteOwner = result.authority, result.owner
			}
		}
		if lookupErr != nil || remoteOwner == nil {
			return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, lookupErr)
		}
	}
	query.Owner = *remoteOwner
	response, err := store.videoForwarder.Query(ctx, authority, query)
	if err != nil {
		return nil, true, errors.Join(service.ErrUpstreamKeyPolicy, err)
	}
	return response, true, nil
}
