// Package config owns the on-disk layout of a node's state.
//
// Two YAML files live under the data directory:
//
//	node.yaml     — local identity, never replicated (id, addresses, key paths)
//	cluster.yaml  — replicated state (peers, checks, alerts, version)
//	trust.yaml    — local fingerprint trust store
//	keys/         — RSA private + public keys + self-signed cert
//
// A unix socket for the local CLI lives alongside (defaults to
// /var/run/quptime/quptime.sock when running as root, otherwise
// $XDG_RUNTIME_DIR/quptime/quptime.sock).
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Default file names. Callers should always go through DataDir() so an
// override via QUPTIME_DIR is respected.
const (
	NodeFile    = "node.yaml"
	ClusterFile = "cluster.yaml"
	TrustFile   = "trust.yaml"
	KeysDir     = "keys"
	PrivateKey  = "private.pem"
	PublicKey   = "public.pem"
	CertFile    = "cert.pem"
	SocketName  = "quptime.sock"

	envDataDir = "QUPTIME_DIR"
)

// DataDir returns the configured data directory. Order of resolution:
//  1. $QUPTIME_DIR if set
//  2. /etc/quptime when running as root
//  3. $XDG_CONFIG_HOME/quptime (or ~/.config/quptime) otherwise
func DataDir() string {
	if v := os.Getenv(envDataDir); v != "" {
		return v
	}
	if os.Geteuid() == 0 {
		return "/etc/quptime"
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "quptime")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "./quptime"
	}
	return filepath.Join(home, ".config", "quptime")
}

// SocketPath returns the unix socket used for local CLI ↔ daemon control.
//
// Resolution order:
//  1. $QUPTIME_SOCKET — explicit operator override.
//  2. $RUNTIME_DIRECTORY — set by systemd when the unit declares
//     RuntimeDirectory=quptime. This is the path the daemon uses
//     when run under the packaged unit: /run/quptime/quptime.sock.
//  3. The canonical system socket path — /run/quptime/quptime.sock —
//     if it exists. This catches the CLI side regardless of who is
//     invoking it: `sudo -u quptime qu status` strips RUNTIME_DIRECTORY
//     and XDG_RUNTIME_DIR, so without this probe the CLI falls all
//     the way through to /tmp/quptime-<user>/… and reports "no such
//     file" even while the daemon is happily listening.
//  4. /var/run/quptime/… when euid is 0 (CLI side, packaged installs
//     on systems where /var/run isn't a symlink to /run).
//  5. $XDG_RUNTIME_DIR/quptime/… for user-mode installs.
//  6. /tmp/quptime-<user>/… as a last resort.
func SocketPath() string {
	if v := os.Getenv("QUPTIME_SOCKET"); v != "" {
		return v
	}
	if v := os.Getenv("RUNTIME_DIRECTORY"); v != "" {
		// systemd may pass multiple colon-separated entries when more
		// than one RuntimeDirectory= is declared. Ours is single, but
		// be defensive in case a future unit adds more.
		if i := strings.IndexByte(v, ':'); i >= 0 {
			v = v[:i]
		}
		return filepath.Join(v, SocketName)
	}
	// If a system-managed daemon is already listening, route there
	// regardless of euid. Without this, `sudo -u quptime qu …` can't
	// find the socket the daemon (also running as quptime) created
	// via RuntimeDirectory=.
	for _, p := range []string{
		"/run/quptime/" + SocketName,
		"/var/run/quptime/" + SocketName,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if os.Geteuid() == 0 {
		return "/var/run/quptime/" + SocketName
	}
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "quptime", SocketName)
	}
	return filepath.Join(os.TempDir(), "quptime-"+envUserSuffix(), SocketName)
}

func envUserSuffix() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "default"
}

// NodeFilePath returns the absolute path to node.yaml.
func NodeFilePath() string { return filepath.Join(DataDir(), NodeFile) }

// ClusterFilePath returns the absolute path to cluster.yaml.
func ClusterFilePath() string { return filepath.Join(DataDir(), ClusterFile) }

// TrustFilePath returns the absolute path to trust.yaml.
func TrustFilePath() string { return filepath.Join(DataDir(), TrustFile) }

// PrivateKeyPath returns the absolute path to the RSA private key.
func PrivateKeyPath() string { return filepath.Join(DataDir(), KeysDir, PrivateKey) }

// PublicKeyPath returns the absolute path to the RSA public key.
func PublicKeyPath() string { return filepath.Join(DataDir(), KeysDir, PublicKey) }

// CertFilePath returns the absolute path to the self-signed cert (PEM).
func CertFilePath() string { return filepath.Join(DataDir(), KeysDir, CertFile) }

// EnsureDataDir creates the data directory tree if absent.
func EnsureDataDir() error {
	dir := DataDir()
	if err := os.MkdirAll(filepath.Join(dir, KeysDir), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Dir(SocketPath()), 0o700)
}

// AtomicWrite writes data to path through a temp file + rename. The temp
// file is created in the same directory so the rename is atomic on POSIX.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return errors.New("empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
