package video

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

const ForwardPrefix = "/internal/regional/video"
const forwardMaxBody = 16 << 10

// ForwardConfig provisions a separate, bidirectional media data plane. Control
// credentials and the public application listener are never reused implicitly.
type ForwardConfig struct {
	Listen, Endpoint, CAFile, CertFile, KeyFile, PeerPin string
	UserIDs, APIKeyIDs, AccountIDs                       []int64
	TimeoutSeconds                                       int
}

type Locate struct {
	Principal Owner  `json:"principal"`
	TaskID    string `json:"task_id"`
}

type Location struct {
	Authority string `json:"authority"`
	Owner     Owner  `json:"owner"`
	TaskID    string `json:"task_id"`
}

type Query struct {
	Owner      Owner  `json:"owner"`
	TaskID     string `json:"task_id"`
	Operation  string `json:"operation"`
	Credential string `json:"credential"`
	ClientIP   string `json:"client_ip"`
	UserAgent  string `json:"user_agent"`
	Range      string `json:"range"`
}

func (q Query) Valid() bool {
	provider := ""
	switch q.Operation {
	case "video_status", "video_content":
		provider = "grok"
	case "seedance_status", "seedance_delete":
		provider = "seedance"
	case "minimax_h3_status", "minimax_h3_content":
		provider = "minimax-h3"
	}
	_, err := netip.ParseAddr(q.ClientIP)
	return provider != "" && q.Owner.Valid() && q.Owner.Provider == provider && validForwardText(q.TaskID, 256, true) &&
		validForwardText(q.Credential, 8192, true) && validForwardText(q.UserAgent, 512, false) &&
		validForwardText(q.Range, 256, false) && err == nil
}

func validForwardText(s string, max int, required bool) bool {
	return (!required || s != "") && len(s) <= max && s == strings.TrimSpace(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

func SamePrincipal(a, b Owner) bool {
	return a.ValidPrincipal() && b.ValidPrincipal() && a.UserID == b.UserID && a.APIKeyID == b.APIKeyID && a.GroupID == b.GroupID && a.Provider == b.Provider
}

// Dispatch must re-enter the original authenticated application handler. It
// receives no upstream credential, target URL, lease, or generation operation.
type Dispatch func(http.ResponseWriter, *http.Request, Query)

type Forwarder struct {
	config     ForwardConfig
	authority  string
	peer       string
	resolve    func(context.Context, Owner, string) (*Owner, error)
	dispatch   atomic.Pointer[Dispatch]
	client     *http.Client
	server     *http.Server
	listener   net.Listener
	peerDigest []byte
}

func NewForwarder(cfg ForwardConfig, authority, peer string, resolve func(context.Context, Owner, string) (*Owner, error)) (*Forwarder, error) {
	u, err := url.Parse(cfg.Endpoint)
	pin, pinErr := hex.DecodeString(cfg.PeerPin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || cfg.Listen == "" || pinErr != nil || len(pin) != sha256.Size || cfg.PeerPin != hex.EncodeToString(pin) ||
		!ValidAuthority(authority) || !ValidAuthority(peer) || authority == peer || (authority != PrimaryAuthority && peer != PrimaryAuthority) || resolve == nil {
		return nil, ErrUnavailable
	}
	for _, ids := range [][]int64{cfg.UserIDs, cfg.APIKeyIDs, cfg.AccountIDs} {
		seen := map[int64]bool{}
		if len(ids) == 0 {
			return nil, ErrUnavailable
		}
		for _, id := range ids {
			if id <= 0 || seen[id] {
				return nil, ErrConflict
			}
			seen[id] = true
		}
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 120
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 600 {
		return nil, ErrUnavailable
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, ErrUnavailable
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, ErrUnavailable
	}
	identity, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, ErrUnavailable
	}
	leaf, err := x509.ParseCertificate(identity.Certificate[0])
	if err != nil || (!slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) &&
		(!slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth))) {
		return nil, ErrUnavailable
	}
	f := &Forwarder{config: cfg, authority: authority, peer: peer, resolve: resolve, peerDigest: pin}
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 15 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, RootCAs: roots, VerifyConnection: f.verifyPeer}}
	f.client = &http.Client{Transport: transport, Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	f.listener, err = net.Listen("tcp", cfg.Listen)
	if err != nil {
		f.client.CloseIdleConnections()
		return nil, ErrUnavailable
	}
	f.server = &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: time.Duration(cfg.TimeoutSeconds) * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Certificates: []tls.Certificate{identity}}}
	return f, nil
}

func (f *Forwarder) verifyPeer(state tls.ConnectionState) error {
	if state.Version < tls.VersionTLS13 || len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
		return ErrUnavailable
	}
	sum := sha256.Sum256(state.PeerCertificates[0].Raw)
	if subtle.ConstantTimeCompare(sum[:], f.peerDigest) != 1 {
		return ErrConflict
	}
	return nil
}

func (f *Forwarder) principalAllowed(o Owner) bool {
	return o.ValidPrincipal() && slices.Contains(f.config.UserIDs, o.UserID) && slices.Contains(f.config.APIKeyIDs, o.APIKeyID)
}

func (f *Forwarder) SetDispatch(dispatch Dispatch) {
	f.dispatch.Store(&dispatch)
}

func (f *Forwarder) Run(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- f.server.Serve(tls.NewListener(f.listener, f.server.TLSConfig)) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := f.server.Shutdown(shutdown)
		if err != nil {
			_ = f.server.Close()
		}
		f.client.CloseIdleConnections()
		return err
	}
}

func (f *Forwarder) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.TLS == nil || f.verifyPeer(*r.TLS) != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.URL.RawQuery != "" || (r.URL.Path != ForwardPrefix+"/locate" && r.URL.Path != ForwardPrefix+"/query") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, forwardMaxBody)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	var lookup Locate
	var query Query
	var payload any = &lookup
	if r.URL.Path == ForwardPrefix+"/query" {
		payload = &query
	}
	var extra any
	if d.Decode(payload) != nil || d.Decode(&extra) != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if payload == &query {
		if !query.Valid() || !slices.Contains(f.config.AccountIDs, query.Owner.AccountID) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		lookup = Locate{Principal: query.Owner, TaskID: query.TaskID}
	}
	if !f.principalAllowed(lookup.Principal) || !validForwardText(lookup.TaskID, 256, true) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	owner, err := f.resolve(r.Context(), lookup.Principal, lookup.TaskID)
	if err != nil || owner == nil || !owner.Valid() || !SamePrincipal(lookup.Principal, *owner) || !slices.Contains(f.config.AccountIDs, owner.AccountID) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("X-Regional-Video-Authority", f.authority)
	if payload == &query {
		if *owner != query.Owner {
			w.WriteHeader(http.StatusConflict)
			return
		}
		dispatch := f.dispatch.Load()
		if dispatch == nil || *dispatch == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		(*dispatch)(w, r, query)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Location{Authority: f.authority, Owner: *owner, TaskID: lookup.TaskID})
}

func (f *Forwarder) post(ctx context.Context, path string, payload any) (*http.Response, error) {
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > forwardMaxBody {
		return nil, ErrUnavailable
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.config.Endpoint, "/")+ForwardPrefix+path, bytes.NewReader(raw))
	if err != nil {
		return nil, ErrUnavailable
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := f.client.Do(r)
	if err != nil {
		return nil, ErrUnavailable
	}
	if response.Header.Get("X-Regional-Video-Authority") != f.peer {
		_ = response.Body.Close()
		return nil, ErrUnavailable
	}
	return response, nil
}

func (f *Forwarder) Locate(ctx context.Context, principal Owner, taskID string) (*Owner, error) {
	if !f.principalAllowed(principal) || !validForwardText(taskID, 256, true) {
		return nil, ErrConflict
	}
	response, err := f.post(ctx, "/locate", Locate{Principal: principal, TaskID: taskID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, forwardMaxBody+1))
	var location Location
	if err != nil || len(raw) > forwardMaxBody || response.StatusCode != http.StatusOK || json.Unmarshal(raw, &location) != nil ||
		location.Authority != f.peer || location.TaskID != taskID || !location.Owner.Valid() || !SamePrincipal(principal, location.Owner) ||
		!slices.Contains(f.config.AccountIDs, location.Owner.AccountID) {
		return nil, ErrUnavailable
	}
	return &location.Owner, nil
}

func (f *Forwarder) Query(ctx context.Context, query Query) (*http.Response, error) {
	if !query.Valid() || !f.principalAllowed(query.Owner) || !slices.Contains(f.config.AccountIDs, query.Owner.AccountID) {
		return nil, ErrConflict
	}
	response, err := f.post(ctx, "/query", query)
	if err != nil {
		return nil, err
	}
	// The MiniMax content handler validates and returns a 302 download URL.
	// Preserve that application response without ever following the redirect;
	// every other node redirect remains a protocol failure.
	if response.StatusCode >= 300 && response.StatusCode < 400 &&
		(query.Operation != "minimax_h3_content" || response.StatusCode != http.StatusFound) {
		_ = response.Body.Close()
		return nil, ErrUnavailable
	}
	return response, nil
}

// Close releases a prepared listener if the containing application never ran.
func (f *Forwarder) Close() error {
	f.client.CloseIdleConnections()
	return errors.Join(f.listener.Close(), f.server.Close())
}
