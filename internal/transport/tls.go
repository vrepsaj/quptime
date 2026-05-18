package transport

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	"git.cer.sh/axodouble/quptime/internal/trust"
)

// MinTLS is the minimum protocol version both sides require.
const MinTLS = tls.VersionTLS13

// TLSAssets bundles the on-disk material needed to spin up either a
// listener or a dialer. Build it once at daemon start and pass to
// ServerConfig / ClientConfig.
type TLSAssets struct {
	Cert  []byte // PEM-encoded leaf cert
	Key   *rsa.PrivateKey
	Trust *trust.Store
}

// tlsCert wraps the local PEM cert + RSA key into a tls.Certificate.
func (a *TLSAssets) tlsCert() (tls.Certificate, error) {
	block, _ := pem.Decode(a.Cert)
	if block == nil {
		return tls.Certificate{}, errors.New("cert PEM has no block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse leaf: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{block.Bytes},
		PrivateKey:  a.Key,
		Leaf:        leaf,
	}, nil
}

// ServerConfig produces a tls.Config suitable for an inter-node
// listener.
//
// We accept any client certificate at the TLS layer (no CA verification
// and no fingerprint pinning here). Trust is enforced one layer up by
// the RPC dispatcher: untrusted peers may only invoke MethodEnroll
// (the pre-deployment-token bootstrap step) or MethodJoin (kept as a
// deprecation-error stub for older binaries). This avoids the
// chicken-and-egg where enrolment itself would need pre-existing
// symmetric trust to complete the handshake.
func (a *TLSAssets) ServerConfig() (*tls.Config, error) {
	cert, err := a.tlsCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         MinTLS,
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true, // trust is gated per-method by the RPC dispatcher
	}, nil
}

// ClientConfig produces a tls.Config suitable for dialing a peer.
// expectedNodeID is optional: if non-empty, the handshake also
// verifies that the cert's fingerprint matches the trust entry for
// that node ID.
func (a *TLSAssets) ClientConfig(expectedNodeID string) (*tls.Config, error) {
	cert, err := a.tlsCert()
	if err != nil {
		return nil, err
	}
	verify := a.Trust.VerifyPeerCert
	if expectedNodeID != "" {
		verify = a.makeStrictVerifier(expectedNodeID)
	}
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		MinVersion:            MinTLS,
		InsecureSkipVerify:    true, // we do our own pinning via VerifyPeerCertificate
		VerifyPeerCertificate: verify,
	}, nil
}

// InsecureBootstrapConfig is the client-side TLS config used only by
// the TOFU prefetch (FetchPeerCert). It accepts any peer cert because
// the caller has not yet established trust; the certificate is
// surfaced to the operator for manual approval before being added to
// the store. Never use this anywhere else.
func (a *TLSAssets) InsecureBootstrapConfig() (*tls.Config, error) {
	cert, err := a.tlsCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         MinTLS,
		InsecureSkipVerify: true,
	}, nil
}

// makeStrictVerifier returns a VerifyPeerCertificate callback that
// pins the connection to the trust entry of a specific node ID.
func (a *TLSAssets) makeStrictVerifier(expectedNodeID string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("peer presented no certificate")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parse peer cert: %w", err)
		}
		entry, ok := a.Trust.Get(expectedNodeID)
		if !ok {
			return fmt.Errorf("no trust entry for node %s", expectedNodeID)
		}
		got := fingerprintOf(cert)
		if got != entry.Fingerprint {
			return fmt.Errorf("fingerprint mismatch for %s: got %s want %s",
				expectedNodeID, got, entry.Fingerprint)
		}
		return nil
	}
}
