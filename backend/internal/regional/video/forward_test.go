package video

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/testutil/mtls"
	"github.com/stretchr/testify/require"
)

func forwardPair(t *testing.T, resolve func(context.Context, Owner, string) (*Owner, error)) (*Forwarder, *Forwarder) {
	t.Helper()
	identities := mtls.Pair(t)
	configs := make([]ForwardConfig, 2)
	for i, identity := range identities {
		configs[i] = ForwardConfig{Listen: "127.0.0.1:0", Endpoint: "https://127.0.0.1:1", UserIDs: []int64{1}, APIKeyIDs: []int64{2}, AccountIDs: []int64{3}, TimeoutSeconds: 5,
			CAFile: identity.CAFile, CertFile: identity.CertFile, KeyFile: identity.KeyFile, PeerPin: identities[1-i].Pin}
	}
	primary, err := NewForwarder(configs[0], PrimaryAuthority, LocalAuthority("us"), resolve)
	require.NoError(t, err)
	local, err := NewForwarder(configs[1], LocalAuthority("us"), PrimaryAuthority, resolve)
	require.NoError(t, err)
	primary.config.Endpoint = "https://" + local.listener.Addr().String()
	local.config.Endpoint = "https://" + primary.listener.Addr().String()
	for _, forwarder := range []*Forwarder{primary, local} {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- forwarder.Run(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(8 * time.Second):
				t.Error("video forwarder did not stop")
			}
		})
	}
	return primary, local
}

func TestVideoForwardingPinnedBidirectional(t *testing.T) {
	owner := directoryFixture().Entries[0].Owner
	var resolves, dispatches atomic.Int32
	resolve := func(_ context.Context, principal Owner, task string) (*Owner, error) {
		resolves.Add(1)
		if !SamePrincipal(principal, owner) || task != "task-fixture" {
			return nil, ErrUnavailable
		}
		return &owner, nil
	}
	primary, local := forwardPair(t, resolve)
	data := bytes.Repeat([]byte("video-data"), 20_000)
	for _, f := range []*Forwarder{primary, local} {
		f.SetDispatch(func(w http.ResponseWriter, _ *http.Request, q Query) {
			dispatches.Add(1)
			if q.Credential != "downstream-fixture" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if q.Originator != "codex_cli_rs" || q.Model != "model-fixture" || !q.Subagent {
				t.Error("policy evidence must survive the mutual-TLS hop")
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(data)
		})
	}
	for _, f := range []*Forwarder{primary, local} {
		principal := owner
		principal.AccountID, principal.RouteFingerprint = 0, [32]byte{}
		got, err := f.Locate(t.Context(), principal, "task-fixture")
		require.NoError(t, err)
		require.Equal(t, owner, *got)
		q := Query{PolicyVersion: ForwardPolicyVersion, Owner: *got, TaskID: "task-fixture", Operation: "video_content", Credential: "downstream-fixture", ClientIP: "127.0.0.1", Originator: "codex_cli_rs", Model: "model-fixture", Subagent: true}
		response, err := f.Query(t.Context(), q)
		require.NoError(t, err)
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, data, raw, "media responses are streamed, not truncated to the metadata limit")
		require.Equal(t, "video/mp4", response.Header.Get("Content-Type"))
	}
	require.EqualValues(t, 4, resolves.Load(), "both locate and physical query recheck the original owner")
	require.EqualValues(t, 2, dispatches.Load())
	// The previous wire contract had no policy version or evidence fields.
	// Each direction must reject mixed versions instead of dropping evidence.
	type legacyQuery struct {
		Owner      Owner  `json:"owner"`
		TaskID     string `json:"task_id"`
		Operation  string `json:"operation"`
		Credential string `json:"credential"`
		ClientIP   string `json:"client_ip"`
		UserAgent  string `json:"user_agent"`
		Range      string `json:"range"`
	}
	legacy := legacyQuery{Owner: owner, TaskID: "task-fixture", Operation: "video_content", Credential: "downstream-fixture", ClientIP: "127.0.0.1"}
	_, err := local.post(t.Context(), "/query", legacy)
	require.ErrorIs(t, err, ErrUnavailable, "new origins must reject a legacy peer lacking policy evidence")
	current := Query{PolicyVersion: ForwardPolicyVersion, Owner: owner, TaskID: legacy.TaskID, Operation: legacy.Operation, Credential: legacy.Credential, ClientIP: legacy.ClientIP}
	raw, err := json.Marshal(current)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	require.Error(t, decoder.Decode(&legacy), "old strict decoders must reject the new policy version")
	require.EqualValues(t, 4, resolves.Load())
	require.EqualValues(t, 2, dispatches.Load())
	for _, mutation := range []string{"user", "key", "group", "account", "route", "task", "generation", "credential-injection", "client-ip", "originator-injection", "originator-oversize", "model-injection", "model-oversize", "old-policy", "future-policy"} {
		t.Run(mutation, func(t *testing.T) {
			q := Query{PolicyVersion: ForwardPolicyVersion, Owner: owner, TaskID: "task-fixture", Operation: "video_content", Credential: "downstream-fixture", ClientIP: "127.0.0.1"}
			switch mutation {
			case "user":
				q.Owner.UserID++
			case "key":
				q.Owner.APIKeyID++
			case "group":
				q.Owner.GroupID++
			case "account":
				q.Owner.AccountID++
			case "route":
				q.Owner.RouteFingerprint[0]++
			case "task":
				q.TaskID += "-other"
			case "generation":
				q.Operation = "videos_generations"
			case "credential-injection":
				q.Credential += "\r\nInjected: true"
			case "client-ip":
				q.ClientIP = "127.0.0.1, 8.8.8.8"
			case "originator-injection":
				q.Originator = "ok\r\nInjected: true"
			case "originator-oversize":
				q.Originator = strings.Repeat("o", 513)
			case "model-injection":
				q.Model = "ok\nInjected"
			case "model-oversize":
				q.Model = strings.Repeat("m", 513)
			case "old-policy":
				q.PolicyVersion = 0
			case "future-policy":
				q.PolicyVersion++
			}
			response, err := local.Query(t.Context(), q)
			if err == nil {
				require.NotNil(t, response)
				require.GreaterOrEqual(t, response.StatusCode, 400)
				require.NoError(t, response.Body.Close())
			}
			require.EqualValues(t, 2, dispatches.Load(), "rejected data must never enter the application handler")
		})
	}
	local.peerDigest = bytes.Repeat([]byte{7}, sha256.Size)
	local.client.CloseIdleConnections()
	_, err = local.Locate(t.Context(), owner, "task-fixture")
	require.Error(t, err, "a trusted CA without the assigned leaf pin must not authorize a peer")
}

func TestVideoForwardingCancellationAndTarget(t *testing.T) {
	owner := directoryFixture().Entries[0].Owner
	primary, local := forwardPair(t, func(context.Context, Owner, string) (*Owner, error) { return &owner, nil })
	stopped := make(chan struct{})
	primary.SetDispatch(func(w http.ResponseWriter, r *http.Request, _ Query) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-r.Context().Done()
		close(stopped)
	})
	ctx, cancel := context.WithCancel(t.Context())
	q := Query{PolicyVersion: ForwardPolicyVersion, Owner: owner, TaskID: "task-fixture", Operation: "video_content", Credential: "downstream-fixture", ClientIP: "127.0.0.1"}
	response, err := local.Query(ctx, q)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(response.Body)
	require.Error(t, err)
	require.NoError(t, response.Body.Close())
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reach the original node")
	}
	primary.SetDispatch(func(w http.ResponseWriter, _ *http.Request, _ Query) {
		w.Header().Set("Location", "https://unexpected.example")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, err = local.Query(t.Context(), q)
	require.Error(t, err, "forwarding must never follow another node's redirect")
}

func TestVideoForwardingConfigRejectsUntrustedEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1", "https://user:pass@host", "https://host/path", "https://host?query=1", "https://host#fragment"} {
		_, err := NewForwarder(ForwardConfig{Endpoint: endpoint, Listen: "127.0.0.1:0", PeerPin: strings.Repeat("a", 64)}, PrimaryAuthority, LocalAuthority("us"), func(context.Context, Owner, string) (*Owner, error) { return nil, nil })
		require.Error(t, err)
	}
}

func TestVideoForwardingPreservesMiniMaxContentRedirect(t *testing.T) {
	owner := directoryFixture().Entries[0].Owner
	owner.Provider = "minimax-h3"
	primary, local := forwardPair(t, func(context.Context, Owner, string) (*Owner, error) { return &owner, nil })
	var calls atomic.Int32
	primary.SetDispatch(func(w http.ResponseWriter, _ *http.Request, _ Query) {
		calls.Add(1)
		w.Header().Set("Location", "https://video.example/output.mp4")
		w.WriteHeader(http.StatusFound)
	})
	query := Query{PolicyVersion: ForwardPolicyVersion, Owner: owner, TaskID: "minimax-h3:task", Operation: "minimax_h3_content", Credential: "downstream-fixture", ClientIP: "127.0.0.1"}
	response, err := local.Query(t.Context(), query)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusFound, response.StatusCode)
	require.Equal(t, "https://video.example/output.mp4", response.Header.Get("Location"))
	require.EqualValues(t, 1, calls.Load(), "return the original download response without following its URL")
}
