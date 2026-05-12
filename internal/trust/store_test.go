package trust

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"git.cer.sh/axodouble/quptime/internal/crypto"
)

func TestRoundtripAndLookup(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Error("expected empty store")
	}

	if err := s.Add(Entry{NodeID: "n1", Address: "10.0.0.1:9901", Fingerprint: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Entry{NodeID: "n2", Address: "10.0.0.2:9901", Fingerprint: "sha256:def"}); err != nil {
		t.Fatal(err)
	}

	s2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.List()) != 2 {
		t.Errorf("got %d entries after reload", len(s2.List()))
	}
	if e, ok := s2.Get("n1"); !ok || e.Fingerprint != "sha256:abc" {
		t.Errorf("Get(n1) = %+v ok=%v", e, ok)
	}
	if e, ok := s2.LookupByFingerprint("sha256:def"); !ok || e.NodeID != "n2" {
		t.Errorf("LookupByFingerprint = %+v ok=%v", e, ok)
	}

	removed, err := s2.Remove("n1")
	if err != nil || !removed {
		t.Fatalf("Remove returned %v err=%v", removed, err)
	}
	if _, ok := s2.Get("n1"); ok {
		t.Error("entry still present after Remove")
	}

	s3, _ := Load()
	if _, ok := s3.Get("n1"); ok {
		t.Error("Remove did not persist")
	}
}

func TestAddRequiresIDAndFingerprint(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	s, _ := Load()
	if err := s.Add(Entry{NodeID: "n1"}); err == nil {
		t.Error("missing fingerprint should error")
	}
	if err := s.Add(Entry{Fingerprint: "fp"}); err == nil {
		t.Error("missing node id should error")
	}
}

func TestVerifyPeerCertPinsFingerprint(t *testing.T) {
	t.Setenv("QUPTIME_DIR", t.TempDir())
	if _, err := crypto.GenerateKeyPair("peer-1"); err != nil {
		t.Fatal(err)
	}
	certPEM, _ := crypto.LoadCertPEM()
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)
	fp := crypto.Fingerprint(cert)

	s, _ := Load()

	// Untrusted: should reject.
	if err := s.VerifyPeerCert([][]byte{cert.Raw}, nil); err == nil {
		t.Error("untrusted cert was accepted")
	}

	if err := s.Add(Entry{NodeID: "peer-1", Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}

	// Now trusted.
	if err := s.VerifyPeerCert([][]byte{cert.Raw}, nil); err != nil {
		t.Errorf("trusted cert rejected: %v", err)
	}

	// No certs presented at all should error.
	if err := s.VerifyPeerCert(nil, nil); err == nil {
		t.Error("empty cert chain was accepted")
	}
}
