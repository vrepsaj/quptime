package checks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"git.cer.sh/axodouble/quptime/internal/config"
)

// tlsServer spins up a localhost TLS listener whose leaf cert has the
// supplied NotBefore/NotAfter, then returns its address so probes can
// hit it. The listener stays up until the test exits.
func tlsServer(t *testing.T, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tls-test"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign cert: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Force the handshake to complete so the client sees the cert.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestTLSProberHappyPath(t *testing.T) {
	addr := tlsServer(t, time.Now().Add(-1*time.Hour), time.Now().Add(60*24*time.Hour))
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckTLS, Target: addr,
		Timeout: 5 * time.Second, TLSWarnDays: 14,
	})
	if !res.OK {
		t.Errorf("expected OK, got %+v", res)
	}
}

func TestTLSProberNearExpiry(t *testing.T) {
	addr := tlsServer(t, time.Now().Add(-1*time.Hour), time.Now().Add(7*24*time.Hour))
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckTLS, Target: addr,
		Timeout: 5 * time.Second, TLSWarnDays: 14,
	})
	if res.OK {
		t.Errorf("expected DOWN for cert <14d from expiry, got %+v", res)
	}
	if !strings.Contains(res.Detail, "expires in") {
		t.Errorf("detail should mention impending expiry, got %q", res.Detail)
	}
}

func TestTLSProberAlreadyExpired(t *testing.T) {
	addr := tlsServer(t, time.Now().Add(-48*time.Hour), time.Now().Add(-1*time.Hour))
	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckTLS, Target: addr,
		Timeout: 5 * time.Second,
	})
	if res.OK {
		t.Errorf("expected DOWN for expired cert, got %+v", res)
	}
	if !strings.Contains(res.Detail, "expired") {
		t.Errorf("detail should mention expiry, got %q", res.Detail)
	}
}

func TestTLSProberDialFailure(t *testing.T) {
	// Listen and immediately close so the address is known-bad.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	res := Run(context.Background(), &config.Check{
		ID: "c", Type: config.CheckTLS, Target: addr,
		Timeout: 1 * time.Second,
	})
	if res.OK {
		t.Errorf("dead address should fail check, got %+v", res)
	}
}

func TestTLSDialAddrShapes(t *testing.T) {
	cases := []struct {
		in       string
		sni      string
		wantAddr string
		wantSNI  string
	}{
		{"example.com", "", "example.com:443", "example.com"},
		{"example.com:8443", "", "example.com:8443", "example.com"},
		{"https://example.com/health", "", "example.com:443", "example.com"},
		{"https://example.com:9443", "alias", "example.com:9443", "alias"},
	}
	for _, tc := range cases {
		addr, sni, err := tlsDialAddr(tc.in, tc.sni)
		if err != nil {
			t.Errorf("%s: unexpected err %v", tc.in, err)
			continue
		}
		if addr != tc.wantAddr || sni != tc.wantSNI {
			t.Errorf("%s: got (%s,%s) want (%s,%s)", tc.in, addr, sni, tc.wantAddr, tc.wantSNI)
		}
	}
}
