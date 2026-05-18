package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/crypto"
	"git.cer.sh/axodouble/quptime/internal/transport"
)

// controlMaxFrame caps unix-socket request/response frames. Generous
// because cluster.yaml snapshots travel over this channel too.
const controlMaxFrame = 16 * 1024 * 1024

// Control method names. Defined as constants so the CLI side cannot
// drift out of sync with the daemon.
const (
	CtrlStatus        = "status"
	CtrlMutate        = "mutate"
	CtrlNodeRemove    = "node.remove"
	CtrlTrustList     = "trust.list"
	CtrlTrustRemove   = "trust.remove"
	CtrlAlertTest     = "alert.test"
	CtrlCheckTest     = "check.test"
	CtrlEnrollCreate  = "enroll.create"
	CtrlEnrollList    = "enroll.list"
	CtrlEnrollApprove = "enroll.approve"
	CtrlEnrollRevoke  = "enroll.revoke"
)

// CtrlRequest is the wire envelope for a CLI ↔ daemon message.
type CtrlRequest struct {
	Method string          `json:"method"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// CtrlResponse carries either an error or a result body.
type CtrlResponse struct {
	Error string          `json:"error,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// MutateBody is the payload of CtrlMutate.
type MutateBody struct {
	Kind    transport.MutationKind `json:"kind"`
	Payload json.RawMessage        `json:"payload"`
}

// MutateResult reports the new cluster version after a successful
// mutation.
type MutateResult struct {
	Version uint64 `json:"version"`
}

// AlertTestBody is the payload of CtrlAlertTest.
type AlertTestBody struct {
	AlertID string `json:"alert_id"`
}

// CheckTestBody is the payload of CtrlCheckTest. State is one of
// "down", "up", "recovered" (empty defaults to "down").
type CheckTestBody struct {
	CheckID string `json:"check_id"`
	State   string `json:"state,omitempty"`
}

// NodeRemoveBody / TrustRemoveBody share the same shape.
type NodeRemoveBody struct {
	NodeID string `json:"node_id"`
}

// EnrollCreateBody is the payload of CtrlEnrollCreate.
type EnrollCreateBody struct {
	Name        string        `json:"name,omitempty"`
	TTL         time.Duration `json:"ttl"`
	AutoApprove bool          `json:"auto_approve,omitempty"`
}

// EnrollCreateResult is what `qu enroll create` displays. Token is the
// base64-encoded payload the joiner runs as `qu enroll join <Token>`.
type EnrollCreateResult struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expires_at"`
	AutoApprove bool      `json:"auto_approve"`
	Version     uint64    `json:"version"`
}

// EnrollListResult mirrors the cluster.yaml view of pending enrollments
// for the CLI / TUI.
type EnrollListResult struct {
	Entries []EnrollListEntry `json:"entries"`
}

// EnrollListEntry is one row in EnrollListResult. SecretHash is
// returned only so the CLI can show a fingerprint-like short prefix —
// the original secret cannot be recovered from it.
type EnrollListEntry struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name,omitempty"`
	AutoApprove bool                     `json:"auto_approve"`
	CreatedBy   string                   `json:"created_by"`
	CreatedAt   time.Time                `json:"created_at"`
	ExpiresAt   time.Time                `json:"expires_at"`
	Pending     *EnrollListPendingJoiner `json:"pending,omitempty"`
}

// EnrollListPendingJoiner is the joiner-side view shown when a token
// has been claimed but not yet approved.
type EnrollListPendingJoiner struct {
	NodeID      string    `json:"node_id"`
	Advertise   string    `json:"advertise"`
	Fingerprint string    `json:"fingerprint"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// EnrollTargetBody is the payload of CtrlEnrollApprove and
// CtrlEnrollRevoke. ID may be the token ID or its Name.
type EnrollTargetBody struct {
	ID string `json:"id"`
}

// controlServer accepts CLI commands over a unix socket.
type controlServer struct {
	d *Daemon

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
}

func newControlServer(d *Daemon) *controlServer {
	return &controlServer{d: d, conns: map[net.Conn]struct{}{}}
}

// Serve binds the unix socket and dispatches commands until ctx is
// cancelled.
func (c *controlServer) Serve(ctx context.Context) error {
	sockPath := config.SocketPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return fmt.Errorf("control socket dir: %w", err)
	}
	// stale socket from a previous crash — unlink before binding
	if fi, err := os.Stat(sockPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(sockPath)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod %s: %w", sockPath, err)
	}
	c.mu.Lock()
	c.ln = ln
	c.mu.Unlock()

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
		go c.handleConn(ctx, conn)
	}
}

// Stop closes the listener and any in-flight connections.
func (c *controlServer) Stop() {
	c.mu.Lock()
	if c.ln != nil {
		_ = c.ln.Close()
	}
	for cn := range c.conns {
		_ = cn.Close()
	}
	c.conns = map[net.Conn]struct{}{}
	c.mu.Unlock()
}

func (c *controlServer) handleConn(ctx context.Context, conn net.Conn) {
	c.mu.Lock()
	c.conns[conn] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.conns, conn)
		c.mu.Unlock()
		_ = conn.Close()
	}()

	body, err := readCtrlFrame(conn)
	if err != nil {
		return
	}
	var req CtrlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		_ = writeCtrlResponse(conn, CtrlResponse{Error: "decode: " + err.Error()})
		return
	}
	resp := c.dispatch(ctx, req)
	_ = writeCtrlResponse(conn, resp)
}

func (c *controlServer) dispatch(ctx context.Context, req CtrlRequest) CtrlResponse {
	switch req.Method {
	case CtrlStatus:
		return ok(c.d.buildStatusForCLI(ctx))

	case CtrlMutate:
		var body MutateBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		ver, err := c.d.replicator.LocalMutate(ctx, body.Kind, body.Payload)
		if err != nil {
			return fail(err)
		}
		return ok(MutateResult{Version: ver})

	case CtrlNodeRemove:
		var body NodeRemoveBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		ver, err := c.d.replicator.LocalMutate(ctx, transport.MutationRemovePeer, body.NodeID)
		if err != nil {
			return fail(err)
		}
		if _, err := c.d.trust.Remove(body.NodeID); err != nil {
			return fail(err)
		}
		return ok(MutateResult{Version: ver})

	case CtrlTrustList:
		return ok(c.d.trust.List())

	case CtrlTrustRemove:
		var body NodeRemoveBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		removed, err := c.d.trust.Remove(body.NodeID)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]bool{"removed": removed})

	case CtrlAlertTest:
		var body AlertTestBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		if err := c.d.dispatcher.Test(body.AlertID); err != nil {
			return fail(err)
		}
		return ok(map[string]string{"status": "sent"})

	case CtrlCheckTest:
		var body CheckTestBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		if err := c.d.dispatcher.TestCheck(body.CheckID, body.State); err != nil {
			return fail(err)
		}
		return ok(map[string]string{"status": "sent"})

	case CtrlEnrollCreate:
		var body EnrollCreateBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		result, err := c.d.enrollCreate(ctx, body)
		if err != nil {
			return fail(err)
		}
		return ok(result)

	case CtrlEnrollList:
		return ok(c.d.enrollList())

	case CtrlEnrollApprove:
		var body EnrollTargetBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		ver, err := c.d.replicator.LocalMutate(ctx, transport.MutationApproveEnrollment, body.ID)
		if err != nil {
			return fail(err)
		}
		return ok(MutateResult{Version: ver})

	case CtrlEnrollRevoke:
		var body EnrollTargetBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		ver, err := c.d.replicator.LocalMutate(ctx, transport.MutationRemoveEnrollment, body.ID)
		if err != nil {
			return fail(err)
		}
		return ok(MutateResult{Version: ver})

	default:
		return CtrlResponse{Error: "unknown method: " + req.Method}
	}
}

// enrollCreate mints a new pre-deployment token, records its hash in
// cluster.yaml, and returns the encoded token string the operator
// hands to the joiner.
//
// The token-string is base64(JSON) and embeds:
//   - the token id + raw secret (the secret is NOT stored anywhere;
//     only its sha256 hash lives in cluster.yaml).
//   - one or more cluster endpoint hints (advertise + fingerprint) so
//     the joiner can dial *and* pin the cluster's TLS cert without an
//     additional out-of-band fingerprint exchange.
//
// Endpoint set is the full snapshot of current peers — if any of them
// is reachable, the joiner can complete enrollment. The fingerprint
// pin is per-endpoint, so a future peer rotation that hasn't yet
// reached every node still validates against whichever endpoint
// answers first.
func (d *Daemon) enrollCreate(ctx context.Context, body EnrollCreateBody) (EnrollCreateResult, error) {
	if body.TTL <= 0 {
		body.TTL = 1 * time.Hour
	}
	if body.TTL > 24*7*time.Hour {
		return EnrollCreateResult{}, errors.New("ttl too long; max 168h")
	}

	id, err := randomEnrollID()
	if err != nil {
		return EnrollCreateResult{}, fmt.Errorf("generate token id: %w", err)
	}
	secret, err := randomEnrollSecret()
	if err != nil {
		return EnrollCreateResult{}, fmt.Errorf("generate token secret: %w", err)
	}
	now := time.Now().UTC()
	entry := config.PendingEnrollment{
		ID:          id,
		Name:        body.Name,
		SecretHash:  hashEnrollSecret(secret),
		AutoApprove: body.AutoApprove,
		CreatedBy:   d.node.NodeID,
		CreatedAt:   now,
		ExpiresAt:   now.Add(body.TTL),
	}
	ver, err := d.replicator.LocalMutate(ctx, transport.MutationAddEnrollment, entry)
	if err != nil {
		return EnrollCreateResult{}, fmt.Errorf("record enrollment: %w", err)
	}

	endpoints := d.enrollEndpoints()
	token := EncodeEnrollmentToken(EnrollmentTokenPayload{
		ID:        id,
		Secret:    secret,
		Endpoints: endpoints,
		ExpiresAt: entry.ExpiresAt,
	})

	return EnrollCreateResult{
		ID:          id,
		Name:        body.Name,
		Token:       token,
		ExpiresAt:   entry.ExpiresAt,
		AutoApprove: body.AutoApprove,
		Version:     ver,
	}, nil
}

// enrollEndpoints assembles the list of cluster contact hints baked
// into a token. Always includes self (so the joiner has at least one
// reachable peer even on a brand-new cluster). Peers without
// fingerprint or cert material are skipped because the joiner cannot
// validate them.
func (d *Daemon) enrollEndpoints() []EnrollEndpoint {
	selfFP, _ := crypto.FingerprintFromCertPEM(d.assets.Cert)
	out := []EnrollEndpoint{{
		Advertise:   d.node.AdvertiseAddr(),
		Fingerprint: selfFP,
	}}
	seen := map[string]bool{d.node.NodeID: true}
	for _, p := range d.cluster.Snapshot().Peers {
		if seen[p.NodeID] {
			continue
		}
		if p.Advertise == "" || p.Fingerprint == "" {
			continue
		}
		out = append(out, EnrollEndpoint{
			Advertise:   p.Advertise,
			Fingerprint: p.Fingerprint,
		})
		seen[p.NodeID] = true
	}
	return out
}

// enrollList returns the cluster.yaml view of pending enrollments,
// flattened into the CLI-facing shape.
func (d *Daemon) enrollList() EnrollListResult {
	snap := d.cluster.Snapshot()
	out := EnrollListResult{Entries: make([]EnrollListEntry, 0, len(snap.PendingEnrollments))}
	for _, e := range snap.PendingEnrollments {
		row := EnrollListEntry{
			ID:          e.ID,
			Name:        e.Name,
			AutoApprove: e.AutoApprove,
			CreatedBy:   e.CreatedBy,
			CreatedAt:   e.CreatedAt,
			ExpiresAt:   e.ExpiresAt,
		}
		if e.PendingJoin != nil {
			row.Pending = &EnrollListPendingJoiner{
				NodeID:      e.PendingJoin.NodeID,
				Advertise:   e.PendingJoin.Advertise,
				Fingerprint: e.PendingJoin.Fingerprint,
				SubmittedAt: e.PendingJoin.SubmittedAt,
			}
		}
		out.Entries = append(out.Entries, row)
	}
	return out
}

// EnrollmentTokenPayload is the decoded form of a pre-deployment
// token. Lives here (rather than in the transport package) so the CLI
// can encode/decode it without importing transport's wire types.
type EnrollmentTokenPayload struct {
	ID        string           `json:"id"`
	Secret    string           `json:"secret"`
	Endpoints []EnrollEndpoint `json:"endpoints"`
	ExpiresAt time.Time        `json:"expires_at"`
}

// EnrollEndpoint is one entry in the endpoint hints baked into a
// token. The joiner dials Advertise and verifies the peer's leaf cert
// fingerprint matches Fingerprint before sending the EnrollRequest.
type EnrollEndpoint struct {
	Advertise   string `json:"advertise"`
	Fingerprint string `json:"fingerprint"`
}

// EncodeEnrollmentToken serializes a payload into the operator-facing
// token string: base64(json(payload)). URL-safe so it survives a `qu
// enroll join <token>` shell paste without quoting drama.
func EncodeEnrollmentToken(p EnrollmentTokenPayload) string {
	raw, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeEnrollmentToken parses the operator-facing token string.
func DecodeEnrollmentToken(token string) (EnrollmentTokenPayload, error) {
	var p EnrollmentTokenPayload
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return p, fmt.Errorf("decode token: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("parse token: %w", err)
	}
	if p.ID == "" || p.Secret == "" || len(p.Endpoints) == 0 {
		return p, errors.New("malformed token: missing id, secret, or endpoints")
	}
	return p, nil
}

// randomEnrollID returns a short, URL-safe identifier for a new
// token. 6 bytes = 8 base64 chars — plenty of entropy when the secret
// itself is 32 bytes, and short enough to type when an operator wants
// to reference the token by ID.
func randomEnrollID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomEnrollSecret returns 32 bytes of crypto-random data,
// base64-encoded. Long enough that an attacker cannot brute-force the
// stored hash before the token expires.
func randomEnrollSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func ok(v any) CtrlResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		return CtrlResponse{Error: err.Error()}
	}
	return CtrlResponse{Body: raw}
}

func fail(err error) CtrlResponse {
	return CtrlResponse{Error: err.Error()}
}

func writeCtrlResponse(w io.Writer, resp CtrlResponse) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return writeCtrlFrame(w, body)
}

func writeCtrlFrame(w io.Writer, body []byte) error {
	if len(body) > controlMaxFrame {
		return errors.New("control frame too large")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readCtrlFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > controlMaxFrame {
		return nil, errors.New("control frame too large")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
