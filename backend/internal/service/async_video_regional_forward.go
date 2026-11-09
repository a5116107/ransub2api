package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/gin-gonic/gin"
)

type AsyncVideoRegionalForwarder interface {
	ForwardAsyncVideoLookup(context.Context, video.Query) (*http.Response, bool, error)
	SetAsyncVideoForwardHandler(video.Dispatch)
}

type asyncVideoForwardProofKey struct{}

func mediaVideoForwardPath(query video.Query) (string, string, error) {
	endpoint := GrokMediaEndpoint(query.Operation)
	id := query.TaskID
	prefix := "/v1/videos/"
	method := http.MethodGet
	if endpoint.IsSeedance() {
		id = strings.TrimPrefix(id, "seedance:")
		prefix = "/v1/contents/generations/tasks/"
	} else if endpoint.IsMiniMaxH3() {
		id = strings.TrimPrefix(id, "minimax-h3:")
	}
	if !endpoint.IsVideoLookupRequest() || endpoint.AsyncVideoProvider() != query.Owner.Provider || id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\?#") {
		return "", "", ErrUpstreamKeyPolicy
	}
	path := prefix + url.PathEscape(id)
	if endpoint == GrokMediaEndpointVideoContent || endpoint == MiniMaxH3EndpointContent {
		path += "/content"
	}
	if endpoint == SeedanceEndpointDelete {
		method = http.MethodDelete
	}
	return method, path, nil
}

// InstallMediaVideoForwarding preserves the complete original router, including
// authentication, IP policy, local admission, billing and protocol handling.
func (s *OpenAIGatewayService) InstallMediaVideoForwarding(router http.Handler) {
	if s == nil || router == nil {
		return
	}
	controller, ok := s.httpUpstream.(AsyncVideoRegionalForwarder)
	if !ok {
		return
	}
	controller.SetAsyncVideoForwardHandler(func(w http.ResponseWriter, r *http.Request, query video.Query) {
		method, path, err := mediaVideoForwardPath(query)
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), asyncVideoForwardProofKey{}, query)
		inner, err := http.NewRequestWithContext(ctx, method, "http://regional-video.internal"+path, nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inner.RemoteAddr = net.JoinHostPort(query.ClientIP, "0")
		inner.RequestURI = inner.URL.RequestURI()
		inner.Header.Set("Authorization", "Bearer "+query.Credential)
		inner.Header.Set("User-Agent", query.UserAgent)
		if query.Range != "" {
			inner.Header.Set("Range", query.Range)
		}
		router.ServeHTTP(w, inner)
	})
}

// ForwardRegionalVideoQuery runs before local slots or billing are acquired.
// A forwarded request is single-hop and must still match its authenticated user.
func (s *OpenAIGatewayService) ForwardRegionalVideoQuery(c *gin.Context, endpoint GrokMediaEndpoint, taskID string, key *APIKey, userID int64) (bool, error) {
	if s == nil || c == nil || c.Request == nil || key == nil || !endpoint.IsVideoLookupRequest() {
		return false, nil
	}
	principal := video.Owner{UserID: userID, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), Provider: endpoint.AsyncVideoProvider()}
	if proof, ok := c.Request.Context().Value(asyncVideoForwardProofKey{}).(video.Query); ok {
		if !video.SamePrincipal(principal, proof.Owner) || proof.TaskID != taskID || proof.Operation != string(endpoint) {
			return true, ErrUpstreamKeyPolicy
		}
		return false, nil
	}
	controller, ok := s.httpUpstream.(AsyncVideoRegionalForwarder)
	if !ok {
		return false, nil
	}
	credential := ""
	parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		credential = strings.TrimSpace(parts[1])
	}
	if credential == "" {
		credential = c.GetHeader("x-api-key")
	}
	if credential == "" {
		credential = c.GetHeader("x-goog-api-key")
	}
	clientIP := ip.GetSecurityClientIP(c, s.cfg.TrustForwardedIPForAPIKeyACL())
	if binding := SessionBindingFromContext(c.Request.Context()); binding != nil && strings.TrimSpace(binding.IP) != "" {
		clientIP = binding.IP
	}
	query := video.Query{Owner: principal, TaskID: taskID, Operation: string(endpoint), Credential: credential,
		ClientIP: clientIP, UserAgent: strings.TrimSpace(c.Request.UserAgent()), Range: strings.TrimSpace(c.GetHeader("Range"))}
	response, handled, err := controller.ForwardAsyncVideoLookup(c.Request.Context(), query)
	if err != nil || !handled {
		return handled, err
	}
	if response == nil || response.Body == nil {
		return true, ErrUpstreamKeyPolicy
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		location := response.Header.Get("Location")
		if endpoint != MiniMaxH3EndpointContent || response.StatusCode != http.StatusFound || !validMiniMaxH3OutputURL(location) {
			return true, ErrUpstreamKeyPolicy
		}
		c.Header("Location", location)
	}
	for _, header := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Content-Range", "Accept-Ranges", "Cache-Control", "ETag", "Last-Modified", "Retry-After"} {
		for _, value := range response.Header.Values(header) {
			c.Writer.Header().Add(header, value)
		}
	}
	c.Status(response.StatusCode)
	c.Writer.WriteHeaderNow()
	_, err = io.Copy(c.Writer, response.Body)
	return true, err
}

func checkAsyncVideoForwardOwner(ctx context.Context, owner *AsyncVideoKeyOwner, taskID string) error {
	if proof, ok := ctx.Value(asyncVideoForwardProofKey{}).(video.Query); ok && (owner == nil || *owner != proof.Owner || taskID != proof.TaskID) {
		return errors.Join(ErrUpstreamKeyPolicy, video.ErrConflict)
	}
	return nil
}
