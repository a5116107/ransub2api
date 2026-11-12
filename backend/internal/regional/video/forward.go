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
const ForwardPolicyVersion = 1
const forwardMaxBody = 16 << 10
const MaxForwardPeers = 8
const forwardDirectoryMaxBody = 256 << 10

// ForwardConfig provisions a separate, bidirectional media data plane. Control
// credentials and the public application listener are never reused implicitly.
type ForwardConfig struct {
	Listen, CAFile, CertFile, KeyFile string
	Peers                             []ForwardPeer
	TimeoutSeconds                    int
}

type ForwardPeer struct {
	Authority, Endpoint, PeerPin   string
	UserIDs, APIKeyIDs, AccountIDs []int64
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
	PolicyVersion int    `json:"policy_version"`
	Owner         Owner  `json:"owner"`
	TaskID        string `json:"task_id"`
	Operation     string `json:"operation"`
	Credential    string `json:"credential"`
	ClientIP      string `json:"client_ip"`
	UserAgent     string `json:"user_agent"`
	Range         string `json:"range"`
	Originator    string `json:"originator,omitempty"`
	Model         string `json:"model,omitempty"`
	Subagent      bool   `json:"subagent,omitempty"`
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
	return q.PolicyVersion == ForwardPolicyVersion && provider != "" && q.Owner.Valid() && q.Owner.Provider == provider && validForwardText(q.TaskID, 256, true) &&
		validForwardText(q.Credential, 8192, true) && validForwardText(q.UserAgent, 512, false) &&
		validForwardText(q.Range, 256, false) && validForwardText(q.Originator, 512, false) &&
		validForwardText(q.Model, 512, false) && err == nil
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
	authority string
	peers     map[string]*forwardPeer
	resolve   func(context.Context, Owner, string) (*Owner, error)
	export    func(context.Context) (Snapshot, error)
	dispatch  atomic.Pointer[Dispatch]
	server    *http.Server
	listener  net.Listener
}

type forwardPeer struct {
	config ForwardPeer
	digest []byte
	client *http.Client
}

func (p ForwardPeer) principalAllowed(o Owner) bool {
	return o.ValidPrincipal() && slices.Contains(p.UserIDs, o.UserID) && slices.Contains(p.APIKeyIDs, o.APIKeyID)
}

func NewForwarder(cfg ForwardConfig, authority string, resolve func(context.Context, Owner, string) (*Owner, error), export func(context.Context) (Snapshot, error)) (*Forwarder, error) {
	if cfg.Listen == "" || !ValidAuthority(authority) || resolve == nil || export == nil || len(cfg.Peers) == 0 || len(cfg.Peers) > MaxForwardPeers {
		return nil, ErrUnavailable
	}
	peers := make(map[string]*forwardPeer, len(cfg.Peers))
	pins, endpoints := map[string]bool{}, map[string]bool{}
	for _, peer := range cfg.Peers {
		u, err := url.Parse(peer.Endpoint)
		pin, pinErr := hex.DecodeString(peer.PeerPin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			(u.Path != "" && u.Path != "/") || pinErr != nil || len(pin) != sha256.Size || peer.PeerPin != hex.EncodeToString(pin) ||
			!ValidAuthority(peer.Authority) || peer.Authority == authority || peers[peer.Authority] != nil || pins[peer.PeerPin] || endpoints[strings.TrimRight(peer.Endpoint, "/")] {
			return nil, ErrConflict
		}
		for _, ids := range [][]int64{peer.UserIDs, peer.APIKeyIDs, peer.AccountIDs} {
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
		peer.UserIDs, peer.APIKeyIDs, peer.AccountIDs = slices.Clone(peer.UserIDs), slices.Clone(peer.APIKeyIDs), slices.Clone(peer.AccountIDs)
		peers[peer.Authority] = &forwardPeer{config: peer, digest: pin}
		pins[peer.PeerPin], endpoints[strings.TrimRight(peer.Endpoint, "/")] = true, true
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
	self := sha256.Sum256(leaf.Raw)
	if pins[hex.EncodeToString(self[:])] {
		return nil, ErrConflict
	}
	f := &Forwarder{authority: authority, peers: peers, resolve: resolve, export: export}
	for _, peer := range peers {
		transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 15 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, RootCAs: roots,
				VerifyConnection: func(state tls.ConnectionState) error { return verifyForwardPeer(state, peer.digest) }}}
		peer.client = &http.Client{Transport: transport, Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	f.listener, err = net.Listen("tcp", cfg.Listen)
	if err != nil {
		f.closeClients()
		return nil, ErrUnavailable
	}
	f.server = &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: time.Duration(cfg.TimeoutSeconds) * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Certificates: []tls.Certificate{identity}}}
	return f, nil
}

func verifyForwardPeer(state tls.ConnectionState, pin []byte) error {
	if state.Version < tls.VersionTLS13 || len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
		return ErrUnavailable
	}
	sum := sha256.Sum256(state.PeerCertificates[0].Raw)
	if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
		return ErrConflict
	}
	return nil
}

func (f *Forwarder) authenticatedPeer(state tls.ConnectionState) *forwardPeer {
	for _, peer := range f.peers {
		if verifyForwardPeer(state, peer.digest) == nil {
			return peer
		}
	}
	return nil
}

func (f *Forwarder) Authorities() []string {
	authorities := make([]string, 0, len(f.peers))
	for authority := range f.peers {
		authorities = append(authorities, authority)
	}
	slices.Sort(authorities)
	return authorities
}

func (f *Forwarder) Candidates(principal Owner) []string {
	var authorities []string
	for _, authority := range f.Authorities() {
		if f.peers[authority].config.principalAllowed(principal) {
			authorities = append(authorities, authority)
		}
	}
	return authorities
}

func (f *Forwarder) closeClients() {
	for _, peer := range f.peers {
		peer.client.CloseIdleConnections()
	}
}

func (f *Forwarder) SetDispatch(dispatch Dispatch) {
	f.dispatch.Store(&dispatch)
}

func (f *Forwarder) Run(ctx context.Context) error {
	defer f.closeClients()
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
		return err
	}
}

func (f *Forwarder) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.TLS == nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	peer := f.authenticatedPeer(*r.TLS)
	if peer == nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.URL.RawQuery != "" || (r.URL.Path != ForwardPrefix+"/locate" && r.URL.Path != ForwardPrefix+"/query" && r.URL.Path != ForwardPrefix+"/directory") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// A socket write deadline does not cancel SQL or upstream work. Bound the
	// original handler too, even when the receiving node allows a longer wait.
	ctx, cancel := context.WithTimeout(r.Context(), f.server.WriteTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, forwardMaxBody)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	var lookup Locate
	var query Query
	var directory struct{}
	var payload any = &lookup
	switch r.URL.Path {
	case ForwardPrefix + "/query":
		payload = &query
	case ForwardPrefix + "/directory":
		payload = &directory
	}
	var extra any
	if d.Decode(payload) != nil || d.Decode(&extra) != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if payload == &directory {
		snapshot, err := f.export(r.Context())
		if err != nil || snapshot.Authority != f.authority || snapshot.Validate() != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		entries := make([]Entry, 0, len(snapshot.Entries))
		for _, entry := range snapshot.Entries {
			if peer.config.principalAllowed(entry.Owner) && slices.Contains(peer.config.AccountIDs, entry.Owner.AccountID) {
				entries = append(entries, entry)
			}
		}
		snapshot.Entries = entries
		w.Header().Set("X-Regional-Video-Authority", f.authority)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
		return
	}
	if payload == &query {
		if !query.Valid() || !slices.Contains(peer.config.AccountIDs, query.Owner.AccountID) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		lookup = Locate{Principal: query.Owner, TaskID: query.TaskID}
	}
	if !peer.config.principalAllowed(lookup.Principal) || !validForwardText(lookup.TaskID, 256, true) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	owner, err := f.resolve(r.Context(), lookup.Principal, lookup.TaskID)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if owner == nil {
		w.Header().Set("X-Regional-Video-Authority", f.authority)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !owner.Valid() || !SamePrincipal(lookup.Principal, *owner) || !slices.Contains(peer.config.AccountIDs, owner.AccountID) {
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

func (f *Forwarder) post(ctx context.Context, authority, path string, payload any) (*http.Response, error) {
	peer := f.peers[authority]
	if peer == nil {
		return nil, ErrConflict
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > forwardMaxBody {
		return nil, ErrUnavailable
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(peer.config.Endpoint, "/")+ForwardPrefix+path, bytes.NewReader(raw))
	if err != nil {
		return nil, ErrUnavailable
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := peer.client.Do(r)
	if err != nil {
		return nil, ErrUnavailable
	}
	if response.Header.Get("X-Regional-Video-Authority") != authority {
		_ = response.Body.Close()
		return nil, ErrUnavailable
	}
	return response, nil
}

func (f *Forwarder) Locate(ctx context.Context, authority string, principal Owner, taskID string) (*Owner, error) {
	peer := f.peers[authority]
	if peer == nil || !peer.config.principalAllowed(principal) || !validForwardText(taskID, 256, true) {
		return nil, ErrConflict
	}
	response, err := f.post(ctx, authority, "/locate", Locate{Principal: principal, TaskID: taskID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrTaskUnknown
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, forwardMaxBody+1))
	var location Location
	if err != nil || len(raw) > forwardMaxBody || response.StatusCode != http.StatusOK || json.Unmarshal(raw, &location) != nil ||
		location.Authority != authority || location.TaskID != taskID || !location.Owner.Valid() || !SamePrincipal(principal, location.Owner) ||
		!slices.Contains(peer.config.AccountIDs, location.Owner.AccountID) {
		return nil, ErrUnavailable
	}
	return &location.Owner, nil
}

func (f *Forwarder) Query(ctx context.Context, authority string, query Query) (*http.Response, error) {
	peer := f.peers[authority]
	if peer == nil || !query.Valid() || !peer.config.principalAllowed(query.Owner) || !slices.Contains(peer.config.AccountIDs, query.Owner.AccountID) {
		return nil, ErrConflict
	}
	response, err := f.post(ctx, authority, "/query", query)
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
	f.closeClients()
	return errors.Join(f.listener.Close(), f.server.Close())
}

// Snapshot exchanges metadata only. The pin identifies the source and both
// directions enforce its explicit tenant/account assignment before SQL install.
func (f *Forwarder) Snapshot(ctx context.Context, authority string) (Snapshot, error) {
	var snapshot Snapshot
	response, err := f.post(ctx, authority, "/directory", struct{}{})
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return snapshot, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, forwardDirectoryMaxBody+1))
	if err != nil || len(raw) > forwardDirectoryMaxBody {
		return snapshot, ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(&snapshot) != nil || d.Decode(&extra) != io.EOF || snapshot.Authority != authority || snapshot.Validate() != nil {
		return Snapshot{}, ErrConflict
	}
	peer := f.peers[authority]
	for _, entry := range snapshot.Entries {
		if !peer.config.principalAllowed(entry.Owner) || !slices.Contains(peer.config.AccountIDs, entry.Owner.AccountID) {
			return Snapshot{}, ErrConflict
		}
	}
	return snapshot, nil
}
