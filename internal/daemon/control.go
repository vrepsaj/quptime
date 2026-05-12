package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"git.cer.sh/axodouble/quptime/internal/config"
	"git.cer.sh/axodouble/quptime/internal/crypto"
	"git.cer.sh/axodouble/quptime/internal/transport"
	"git.cer.sh/axodouble/quptime/internal/trust"
)

// controlMaxFrame caps unix-socket request/response frames. Generous
// because cluster.yaml snapshots travel over this channel too.
const controlMaxFrame = 16 * 1024 * 1024

// Control method names. Defined as constants so the CLI side cannot
// drift out of sync with the daemon.
const (
	CtrlStatus      = "status"
	CtrlMutate      = "mutate"
	CtrlNodeProbe   = "node.probe"
	CtrlNodeAdd     = "node.add"
	CtrlNodeRemove  = "node.remove"
	CtrlTrustList   = "trust.list"
	CtrlTrustRemove = "trust.remove"
	CtrlAlertTest   = "alert.test"
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

// NodeProbeBody is the payload of CtrlNodeProbe.
type NodeProbeBody struct {
	Address string `json:"address"`
}

// NodeProbeResult lets the CLI show the operator what they're about
// to trust.
type NodeProbeResult struct {
	NodeID      string `json:"node_id"`
	Fingerprint string `json:"fingerprint"`
	CertPEM     string `json:"cert_pem"`
}

// NodeAddBody captures everything the daemon needs once the operator
// has confirmed the fingerprint.
type NodeAddBody struct {
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
}

// NodeAddResult is returned when a peer has been trusted, joined, and
// added to the cluster config.
type NodeAddResult struct {
	NodeID  string `json:"node_id"`
	Version uint64 `json:"version"`
}

// AlertTestBody is the payload of CtrlAlertTest.
type AlertTestBody struct {
	AlertID string `json:"alert_id"`
}

// NodeRemoveBody / TrustRemoveBody share the same shape.
type NodeRemoveBody struct {
	NodeID string `json:"node_id"`
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
		return ok(c.d.buildStatus())

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

	case CtrlNodeProbe:
		var body NodeProbeBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		sample, err := transport.FetchPeerCert(ctx, c.d.assets, body.Address)
		if err != nil {
			return fail(err)
		}
		return ok(NodeProbeResult{
			NodeID:      sample.Cert.Subject.CommonName,
			Fingerprint: sample.Fingerprint,
			CertPEM:     string(sample.CertPEM),
		})

	case CtrlNodeAdd:
		var body NodeAddBody
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return fail(err)
		}
		result, err := c.d.nodeAdd(ctx, body)
		if err != nil {
			return fail(err)
		}
		return ok(result)

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

	default:
		return CtrlResponse{Error: "unknown method: " + req.Method}
	}
}

// nodeAdd is the daemon-side TOFU completion path: it probes the peer
// for its current cert (verifying the fingerprint matches what the
// operator approved), records a trust entry, swaps trust with the
// peer via the Join RPC, and finally proposes a cluster mutation to
// list the peer in cluster.yaml.
func (d *Daemon) nodeAdd(ctx context.Context, body NodeAddBody) (NodeAddResult, error) {
	sample, err := transport.FetchPeerCert(ctx, d.assets, body.Address)
	if err != nil {
		return NodeAddResult{}, fmt.Errorf("re-probe: %w", err)
	}
	if sample.Fingerprint != body.Fingerprint {
		return NodeAddResult{}, fmt.Errorf("fingerprint changed since probe: got %s want %s",
			sample.Fingerprint, body.Fingerprint)
	}
	peerID := sample.Cert.Subject.CommonName
	if peerID == "" {
		return NodeAddResult{}, errors.New("peer cert has no CommonName / NodeID")
	}

	if err := d.trust.Add(trust.Entry{
		NodeID:      peerID,
		Address:     body.Address,
		Fingerprint: sample.Fingerprint,
		CertPEM:     string(sample.CertPEM),
	}); err != nil {
		return NodeAddResult{}, fmt.Errorf("trust add: %w", err)
	}

	// Ask the peer to record us symmetrically.
	myFP, err := crypto.FingerprintFromCertPEM(d.assets.Cert)
	if err != nil {
		return NodeAddResult{}, fmt.Errorf("own fingerprint: %w", err)
	}
	joinReq := transport.JoinRequest{
		NodeID:        d.node.NodeID,
		Advertise:     d.node.AdvertiseAddr(),
		Fingerprint:   myFP,
		CertPEM:       string(d.assets.Cert),
		ClusterSecret: d.node.ClusterSecret,
	}
	var joinResp transport.JoinResponse
	if err := d.client.Call(ctx, peerID, body.Address, transport.MethodJoin, joinReq, &joinResp); err != nil {
		return NodeAddResult{}, fmt.Errorf("join %s: %w", peerID, err)
	}
	if !joinResp.Accepted {
		return NodeAddResult{}, fmt.Errorf("peer rejected join: %s", joinResp.Error)
	}

	// Propose the cluster-config addition. Routed to master via the
	// replicator; if we are the master, applied directly. Including
	// CertPEM lets other peers auto-trust this node once the new
	// cluster.yaml reaches them.
	peerInfo := config.PeerInfo{
		NodeID:      peerID,
		Advertise:   body.Address,
		Fingerprint: sample.Fingerprint,
		CertPEM:     string(sample.CertPEM),
	}
	ver, err := d.replicator.LocalMutate(ctx, transport.MutationAddPeer, peerInfo)
	if err != nil {
		return NodeAddResult{}, fmt.Errorf("propose peer: %w", err)
	}
	return NodeAddResult{NodeID: peerID, Version: ver}, nil
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
