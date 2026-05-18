package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// HandlerFunc is registered by callers for a specific method name. The
// raw JSON request body and the peer's verified node ID are provided.
// The returned value (if any) is JSON-marshalled into the response.
type HandlerFunc func(ctx context.Context, peerNodeID string, payload json.RawMessage) (any, error)

// Server is a registry of method handlers plus an accept loop. It
// owns no business logic; callers register methods and Serve dispatches.
type Server struct {
	assets   *TLSAssets
	handlers map[string]HandlerFunc

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
}

// NewServer constructs a Server with no handlers registered.
func NewServer(assets *TLSAssets) *Server {
	return &Server{
		assets:   assets,
		handlers: map[string]HandlerFunc{},
		conns:    map[net.Conn]struct{}{},
	}
}

// Handle registers fn for the given method name. Replaces any prior
// handler for the same method.
func (s *Server) Handle(method string, fn HandlerFunc) {
	s.handlers[method] = fn
}

// Serve binds the listener at addr and dispatches incoming RPCs until
// Stop is called or the listener errors out.
func (s *Server) Serve(ctx context.Context, addr string) error {
	tlsCfg, err := s.assets.ServerConfig()
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
	}
}

// Stop closes the listener and all in-flight connections. Safe to call
// from any goroutine.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for c := range s.conns {
		_ = c.Close()
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()
}

func (s *Server) trackConn(c net.Conn)   { s.mu.Lock(); s.conns[c] = struct{}{}; s.mu.Unlock() }
func (s *Server) untrackConn(c net.Conn) { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }

func (s *Server) handleConn(ctx context.Context, raw net.Conn) {
	s.trackConn(raw)
	defer func() {
		s.untrackConn(raw)
		_ = raw.Close()
	}()

	tlsConn, ok := raw.(*tls.Conn)
	if !ok {
		return
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return
	}
	state := tlsConn.ConnectionState()
	peerID := peerNodeIDFromConnState(state)
	peerFP := peerFingerprintFromConnState(state)
	trusted := s.peerTrusted(peerFP)

	for {
		body, err := readFrame(tlsConn)
		if err != nil {
			return
		}
		var req requestEnvelope
		if err := json.Unmarshal(body, &req); err != nil {
			_ = writeError(tlsConn, 0, "decode request: "+err.Error())
			return
		}

		// Until the peer is trusted, only bootstrap calls are allowed
		// through: Enroll (the new pre-deployment-token flow) and
		// legacy Join (kept around so an old node can still hit a new
		// node's listener and get a graceful deprecation error).
		// Everything else needs an existing trust relationship.
		if !trusted && req.Method != MethodJoin && req.Method != MethodEnroll {
			_ = writeError(tlsConn, req.ID, "peer not trusted; obtain an enrollment token via `qu enroll create` on the cluster")
			continue
		}

		fn, exists := s.handlers[req.Method]
		if !exists {
			_ = writeError(tlsConn, req.ID, "unknown method: "+req.Method)
			continue
		}

		result, err := fn(ctx, peerID, req.Params)
		if err != nil {
			_ = writeError(tlsConn, req.ID, err.Error())
			continue
		}
		if err := writeResult(tlsConn, req.ID, result); err != nil {
			return
		}

		// A successful bootstrap call may have written the caller into
		// our trust store (auto-approve enrollment, or the legacy
		// Join path while it still works); re-check so subsequent
		// calls on this same connection flow through normally.
		if !trusted && (req.Method == MethodJoin || req.Method == MethodEnroll) {
			trusted = s.peerTrusted(peerFP)
		}
	}
}

// peerTrusted reports whether peerFP is in our trust store. Returns
// false on empty input so a missing/parse-failed cert is never trusted.
func (s *Server) peerTrusted(peerFP string) bool {
	if peerFP == "" {
		return false
	}
	_, ok := s.assets.Trust.LookupByFingerprint(peerFP)
	return ok
}

// Client opens and pools one mTLS connection per peer node ID. Each
// connection serialises outstanding calls under a mutex; concurrent
// calls to different peers proceed in parallel.
type Client struct {
	assets *TLSAssets

	mu    sync.Mutex
	conns map[string]*clientConn // by peer node ID

	nextID atomic.Uint64
}

// NewClient constructs an empty connection pool.
func NewClient(assets *TLSAssets) *Client {
	return &Client{assets: assets, conns: map[string]*clientConn{}}
}

type clientConn struct {
	mu   sync.Mutex
	conn *tls.Conn
}

// Close drops every pooled connection. Safe to call multiple times.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, cc := range c.conns {
		if cc.conn != nil {
			_ = cc.conn.Close()
		}
		delete(c.conns, id)
	}
}

// Call invokes method on the peer at addr (identified by nodeID for
// fingerprint pinning), marshalling params to JSON and unmarshalling
// the result into out. out may be nil if the caller doesn't care.
func (c *Client) Call(ctx context.Context, nodeID, addr, method string, params any, out any) error {
	cc, err := c.getConn(ctx, nodeID, addr)
	if err != nil {
		return err
	}
	if err := c.callOn(ctx, cc, method, params, out); err != nil {
		// drop the connection on error so the next call reconnects fresh
		c.dropConn(nodeID)
		return err
	}
	return nil
}

func (c *Client) callOn(ctx context.Context, cc *clientConn, method string, params any, out any) error {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}
	id := c.nextID.Add(1)
	env := requestEnvelope{ID: id, Method: method, Params: paramsJSON}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}

	cc.mu.Lock()
	defer cc.mu.Unlock()

	if dl, ok := ctx.Deadline(); ok {
		_ = cc.conn.SetDeadline(dl)
		defer func() { _ = cc.conn.SetDeadline(time.Time{}) }()
	}

	if err := writeFrame(cc.conn, body); err != nil {
		return err
	}
	respBody, err := readFrame(cc.conn)
	if err != nil {
		return err
	}
	var resp responseEnvelope
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("remote: %s", resp.Error)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func (c *Client) getConn(ctx context.Context, nodeID, addr string) (*clientConn, error) {
	c.mu.Lock()
	cc, ok := c.conns[nodeID]
	c.mu.Unlock()
	if ok && cc.conn != nil {
		return cc, nil
	}

	tlsCfg, err := c.assets.ClientConfig(nodeID)
	if err != nil {
		return nil, err
	}
	d := tls.Dialer{Config: tlsCfg}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	tc, ok := raw.(*tls.Conn)
	if !ok {
		_ = raw.Close()
		return nil, errors.New("dial returned non-tls conn")
	}
	cc = &clientConn{conn: tc}
	c.mu.Lock()
	if existing, ok := c.conns[nodeID]; ok && existing.conn != nil {
		// concurrent dial — drop ours, reuse existing
		_ = tc.Close()
		c.mu.Unlock()
		return existing, nil
	}
	c.conns[nodeID] = cc
	c.mu.Unlock()
	return cc, nil
}

func (c *Client) dropConn(nodeID string) {
	c.mu.Lock()
	if cc, ok := c.conns[nodeID]; ok {
		if cc.conn != nil {
			_ = cc.conn.Close()
		}
		delete(c.conns, nodeID)
	}
	c.mu.Unlock()
}

// requestEnvelope is the wire shape of an RPC request frame.
type requestEnvelope struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// responseEnvelope is the wire shape of an RPC response frame.
type responseEnvelope struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func writeResult(w io.Writer, id uint64, result any) error {
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return writeError(w, id, "marshal result: "+err.Error())
		}
		raw = b
	}
	body, err := json.Marshal(responseEnvelope{ID: id, Result: raw})
	if err != nil {
		return err
	}
	return writeFrame(w, body)
}

func writeError(w io.Writer, id uint64, msg string) error {
	body, err := json.Marshal(responseEnvelope{ID: id, Error: msg})
	if err != nil {
		return err
	}
	return writeFrame(w, body)
}

// peerNodeIDFromConnState extracts the peer's NodeID from the cert's
// CommonName field. The init flow sets CN to the local NodeID.
func peerNodeIDFromConnState(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	return cs.PeerCertificates[0].Subject.CommonName
}

// peerFingerprintFromConnState computes the SPKI fingerprint of the
// peer's leaf cert, matching the format the trust store stores. An
// empty result means the peer presented no cert.
func peerFingerprintFromConnState(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	return fingerprintOf(cs.PeerCertificates[0])
}
