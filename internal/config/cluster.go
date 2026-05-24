package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// PeerInfo identifies a cluster member as known to all peers.
//
// CertPEM rides along so the daemon can populate trust.yaml when a
// new node joins: a follower receiving an updated cluster.yaml from
// the master trusts the master, and therefore trusts the peer
// certificates it forwards. Without this, mTLS between new and old
// peers would never succeed because neither would have the other in
// its trust store.
type PeerInfo struct {
	NodeID      string `yaml:"node_id"`
	Advertise   string `yaml:"advertise"`
	Fingerprint string `yaml:"fingerprint"`
	CertPEM     string `yaml:"cert_pem,omitempty"`
}

// CheckType enumerates the supported probe kinds.
type CheckType string

const (
	CheckHTTP CheckType = "http"
	CheckTCP  CheckType = "tcp"
	CheckICMP CheckType = "icmp"
	CheckTLS  CheckType = "tls"
	CheckDNS  CheckType = "dns"
)

// Check describes a single monitored target.
type Check struct {
	ID       string        `yaml:"id"`
	Name     string        `yaml:"name"`
	Type     CheckType     `yaml:"type"`
	Target   string        `yaml:"target"`   // URL, host:port, or host
	Interval time.Duration `yaml:"interval"` // default 30s
	Timeout  time.Duration `yaml:"timeout"`  // default 10s

	// HTTP-only options.
	ExpectStatus int    `yaml:"expect_status,omitempty"`
	BodyMatch    string `yaml:"body_match,omitempty"`

	// TLS-only options.
	//
	// TLSWarnDays trips the check when the leaf certificate's
	// NotAfter is within this many days of now. Default 14 when 0.
	// TLSServerName overrides the SNI sent during the handshake;
	// when empty, the host portion of Target is used.
	TLSWarnDays   int    `yaml:"tls_warn_days,omitempty"`
	TLSServerName string `yaml:"tls_server_name,omitempty"`

	// DNS-only options.
	//
	// DNSRecord selects which record type to look up
	// (a | aaaa | cname | mx | txt | ns). Default "a" when empty.
	// DNSResolver is an optional "host:port" of a specific resolver
	// to query (e.g. "1.1.1.1:53"). When empty, the system resolver
	// is used. DNSExpect is an optional substring that must appear
	// in at least one answer for the check to be UP; empty means a
	// non-empty answer set is enough.
	DNSRecord   string `yaml:"dns_record,omitempty"`
	DNSResolver string `yaml:"dns_resolver,omitempty"`
	DNSExpect   string `yaml:"dns_expect,omitempty"`

	// AlertIDs lists which configured alerts fire when this check
	// transitions state.
	AlertIDs []string `yaml:"alert_ids,omitempty"`

	// SuppressAlertIDs lets a check opt out of specific default alerts.
	SuppressAlertIDs []string `yaml:"suppress_alert_ids,omitempty"`

	// Disabled pauses the check: the scheduler skips probing it and the
	// aggregator naturally falls quiet as the existing per-node results
	// age out. Stored as the negation of "enabled" so the zero value of
	// a freshly-added check is enabled, and existing cluster.yaml files
	// without the field continue to behave as before.
	Disabled bool `yaml:"disabled,omitempty"`
}

// AlertType enumerates supported notifier kinds.
type AlertType string

const (
	AlertSMTP    AlertType = "smtp"
	AlertDiscord AlertType = "discord"
)

// Alert describes a single notifier destination.
type Alert struct {
	ID   string    `yaml:"id"`
	Name string    `yaml:"name"`
	Type AlertType `yaml:"type"`

	// Default attaches this alert to every check automatically, on top
	// of any explicit AlertIDs the check lists. A check that wants to
	// opt out of a default alert can list it under SuppressAlertIDs.
	Default bool `yaml:"default,omitempty"`

	// Disabled silences the alert: EffectiveAlertsFor filters it out so
	// it neither fires on transitions nor counts as a default attachment.
	// Stored as the negation of "enabled" so the zero value is enabled.
	Disabled bool `yaml:"disabled,omitempty"`

	// SMTP options.
	SMTPHost     string   `yaml:"smtp_host,omitempty"`
	SMTPPort     int      `yaml:"smtp_port,omitempty"`
	SMTPUser     string   `yaml:"smtp_user,omitempty"`
	SMTPPassword string   `yaml:"smtp_password,omitempty"`
	SMTPFrom     string   `yaml:"smtp_from,omitempty"`
	SMTPTo       []string `yaml:"smtp_to,omitempty"`
	SMTPStartTLS bool     `yaml:"smtp_starttls,omitempty"`

	// Discord options.
	DiscordWebhook string `yaml:"discord_webhook,omitempty"`

	// SubjectTemplate / BodyTemplate are optional text/template strings
	// that override the default rendering. Empty means use the built-in
	// format. Discord ignores SubjectTemplate (it has no subject line);
	// SMTP uses both. Available variables: {{.Check.Name}},
	// {{.Check.Type}}, {{.Check.Target}}, {{.Check.ID}}, {{.From}},
	// {{.To}}, {{.Verb}}, {{.VerbLower}}, {{.Snapshot.Reports}}, {{.Snapshot.OKCount}},
	// {{.Snapshot.NotOK}}, {{.Snapshot.Detail}}, {{.NodeID}}, {{.When}}.
	SubjectTemplate string `yaml:"subject_template,omitempty"`
	BodyTemplate    string `yaml:"body_template,omitempty"`
}

// PendingEnrollment is a pre-deployment authorization token issued by
// the cluster. It lives in the replicated config so any peer can
// validate a presented token (the new node may reach any peer first,
// not necessarily the master).
//
// The Secret on the token-string handed to the operator never lands in
// cluster.yaml — only SecretHash does, so a peer that leaks its
// cluster.yaml does not also leak usable tokens. AutoApprove controls
// whether successful submission immediately adds the joiner as a peer,
// or instead parks the joiner under PendingJoin awaiting a separate
// `qu enroll approve` from a cluster operator.
type PendingEnrollment struct {
	// ID is the public, non-secret identifier for this token. Used by
	// the enroll RPC to look up the entry and by `qu enroll list /
	// approve / revoke` to reference it.
	ID string `yaml:"id"`

	// Name is an optional human-readable label (e.g. "bravo", "us-east-2").
	Name string `yaml:"name,omitempty"`

	// SecretHash is sha256(token.Secret), stored only as the hash so an
	// operator who can read cluster.yaml cannot replay a token.
	SecretHash string `yaml:"secret_hash"`

	// AutoApprove decides what happens when a joiner submits this token:
	// true  → master immediately adds the joiner as a peer and removes
	//         the token. The cluster operator's act of issuing the token
	//         is the approval.
	// false → master records the joiner under PendingJoin and returns
	//         "pending"; an operator must run `qu enroll approve <id>`
	//         to commit.
	AutoApprove bool `yaml:"auto_approve,omitempty"`

	// CreatedBy is the NodeID that issued the token.
	CreatedBy string `yaml:"created_by"`

	// CreatedAt / ExpiresAt scope the token's lifetime. The master
	// rejects submissions after ExpiresAt and prunes expired tokens
	// during normal mutation paths.
	CreatedAt time.Time `yaml:"created_at"`
	ExpiresAt time.Time `yaml:"expires_at"`

	// PendingJoin is set when AutoApprove=false and a joiner has
	// submitted their identity. nil means "token issued, no joiner has
	// claimed it yet". When set, `qu enroll approve <id>` commits this
	// peer into the cluster.
	PendingJoin *PendingJoin `yaml:"pending_join,omitempty"`
}

// PendingJoin is the joiner-side material recorded under a non-auto
// enrollment between submission and operator approval.
type PendingJoin struct {
	NodeID      string    `yaml:"node_id"`
	Advertise   string    `yaml:"advertise"`
	Fingerprint string    `yaml:"fingerprint"`
	CertPEM     string    `yaml:"cert_pem"`
	SubmittedAt time.Time `yaml:"submitted_at"`
}

// ClusterConfig is the replicated cluster state. The Version field
// strictly increases on every mutation; the master is the only node
// that bumps it.
type ClusterConfig struct {
	Version   uint64    `yaml:"version"`
	UpdatedAt time.Time `yaml:"updated_at"`
	UpdatedBy string    `yaml:"updated_by"`

	Peers              []PeerInfo          `yaml:"peers"`
	Checks             []Check             `yaml:"checks"`
	Alerts             []Alert             `yaml:"alerts"`
	PendingEnrollments []PendingEnrollment `yaml:"pending_enrollments,omitempty"`

	mu       sync.RWMutex `yaml:"-"`
	onChange []func()     // fired after any successful Mutate/Replace
	lastSum  [32]byte     // sha256 of the bytes most recently written
}

// OnChange registers a callback fired after every successful Mutate
// or Replace. Callbacks run synchronously on the mutating goroutine
// AFTER the lock is released — they may safely call back into the
// config to read snapshots.
func (c *ClusterConfig) OnChange(fn func()) {
	c.mu.Lock()
	c.onChange = append(c.onChange, fn)
	c.mu.Unlock()
}

func (c *ClusterConfig) fireOnChange() {
	c.mu.RLock()
	cbs := append([]func(){}, c.onChange...)
	c.mu.RUnlock()
	for _, fn := range cbs {
		fn()
	}
}

// LoadClusterConfig reads cluster.yaml. A missing file returns an
// empty (version 0) config — callers should treat that as the
// pre-bootstrap state.
func LoadClusterConfig() (*ClusterConfig, error) {
	raw, err := os.ReadFile(ClusterFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &ClusterConfig{}, nil
		}
		return nil, err
	}
	cfg := &ClusterConfig{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse cluster.yaml: %w", err)
	}
	cfg.lastSum = sha256.Sum256(raw)
	return cfg, nil
}

// Save writes cluster.yaml atomically. Caller is responsible for
// having already taken any external locks.
func (c *ClusterConfig) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := AtomicWrite(ClusterFilePath(), out, 0o600); err != nil {
		return err
	}
	c.lastSum = sha256.Sum256(out)
	return nil
}

// LastSavedSum returns the sha256 of the bytes most recently written
// to disk. The manual-edit watcher uses this to distinguish edits
// originating from this daemon (where the on-disk hash matches) from
// edits made externally by the operator.
func (c *ClusterConfig) LastSavedSum() [32]byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSum
}

// SetLastSavedSum lets the manual-edit watcher record that it has
// observed (and either applied or rejected) a specific on-disk hash,
// so the same edit isn't reprocessed on every poll.
func (c *ClusterConfig) SetLastSavedSum(sum [32]byte) {
	c.mu.Lock()
	c.lastSum = sum
	c.mu.Unlock()
}

// Snapshot returns a deep-enough copy of the config that can be
// safely serialized while the original continues to mutate.
func (c *ClusterConfig) Snapshot() *ClusterConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := &ClusterConfig{
		Version:            c.Version,
		UpdatedAt:          c.UpdatedAt,
		UpdatedBy:          c.UpdatedBy,
		Peers:              append([]PeerInfo(nil), c.Peers...),
		Checks:             append([]Check(nil), c.Checks...),
		Alerts:             append([]Alert(nil), c.Alerts...),
		PendingEnrollments: copyEnrollments(c.PendingEnrollments),
	}
	return cp
}

// copyEnrollments returns a defensive copy of the enrollment slice
// including any PendingJoin pointers (Go slice copy aliases the
// pointer; we want a fresh struct so callers can mutate without
// affecting the live config).
func copyEnrollments(src []PendingEnrollment) []PendingEnrollment {
	if len(src) == 0 {
		return nil
	}
	out := make([]PendingEnrollment, len(src))
	for i, e := range src {
		cp := e
		if e.PendingJoin != nil {
			pj := *e.PendingJoin
			cp.PendingJoin = &pj
		}
		out[i] = cp
	}
	return out
}

// Mutate runs fn under the config write lock, bumps Version on
// success, and writes the file. Only the master should call this.
func (c *ClusterConfig) Mutate(byNode string, fn func(*ClusterConfig) error) error {
	c.mu.Lock()
	if err := fn(c); err != nil {
		c.mu.Unlock()
		return err
	}
	c.Version++
	c.UpdatedAt = time.Now().UTC()
	c.UpdatedBy = byNode
	out, err := yaml.Marshal(c)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := AtomicWrite(ClusterFilePath(), out, 0o600); err != nil {
		c.mu.Unlock()
		return err
	}
	c.lastSum = sha256.Sum256(out)
	c.mu.Unlock()
	c.fireOnChange()
	return nil
}

// Replace overwrites the local config with an incoming snapshot if
// that snapshot has a strictly greater version. Returns true if
// applied.
func (c *ClusterConfig) Replace(incoming *ClusterConfig) (bool, error) {
	c.mu.Lock()
	if incoming.Version <= c.Version {
		c.mu.Unlock()
		return false, nil
	}
	c.Version = incoming.Version
	c.UpdatedAt = incoming.UpdatedAt
	c.UpdatedBy = incoming.UpdatedBy
	c.Peers = append([]PeerInfo(nil), incoming.Peers...)
	c.Checks = append([]Check(nil), incoming.Checks...)
	c.Alerts = append([]Alert(nil), incoming.Alerts...)
	c.PendingEnrollments = copyEnrollments(incoming.PendingEnrollments)
	out, err := yaml.Marshal(c)
	if err != nil {
		c.mu.Unlock()
		return false, err
	}
	if err := AtomicWrite(ClusterFilePath(), out, 0o600); err != nil {
		c.mu.Unlock()
		return false, err
	}
	c.lastSum = sha256.Sum256(out)
	c.mu.Unlock()
	c.fireOnChange()
	return true, nil
}

// EffectiveAlertsFor returns the alerts that should fire when a check
// transitions: every alert explicitly listed in check.AlertIDs, plus
// every alert flagged Default=true, minus anything the check listed
// under SuppressAlertIDs. Result is de-duplicated by alert ID.
func (c *ClusterConfig) EffectiveAlertsFor(check *Check) []Alert {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if check == nil {
		return nil
	}
	suppress := map[string]struct{}{}
	for _, s := range check.SuppressAlertIDs {
		suppress[s] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []Alert

	add := func(a Alert) {
		if a.Disabled {
			return
		}
		if _, dup := seen[a.ID]; dup {
			return
		}
		if _, off := suppress[a.ID]; off {
			return
		}
		if _, off := suppress[a.Name]; off {
			return
		}
		seen[a.ID] = struct{}{}
		out = append(out, a)
	}

	for _, want := range check.AlertIDs {
		for i := range c.Alerts {
			if c.Alerts[i].ID == want || c.Alerts[i].Name == want {
				add(c.Alerts[i])
				break
			}
		}
	}
	for i := range c.Alerts {
		if c.Alerts[i].Default {
			add(c.Alerts[i])
		}
	}
	return out
}

// FindAlert returns the alert with the given ID or name, or nil if
// no entry matches.
func (c *ClusterConfig) FindAlert(idOrName string) *Alert {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.Alerts {
		if c.Alerts[i].ID == idOrName || c.Alerts[i].Name == idOrName {
			cp := c.Alerts[i]
			return &cp
		}
	}
	return nil
}

// FindEnrollment returns the enrollment with the given ID or Name, or
// nil if no entry matches. Result is a copy (PendingJoin too) — safe
// for the caller to mutate.
func (c *ClusterConfig) FindEnrollment(idOrName string) *PendingEnrollment {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.PendingEnrollments {
		e := c.PendingEnrollments[i]
		if e.ID == idOrName || (e.Name != "" && e.Name == idOrName) {
			cp := e
			if e.PendingJoin != nil {
				pj := *e.PendingJoin
				cp.PendingJoin = &pj
			}
			return &cp
		}
	}
	return nil
}

// FindEnrollmentByID returns the entry matching the exact ID. Unlike
// FindEnrollment, it does not fall back to matching Name — useful
// inside the RPC path where the joiner presents the ID verbatim.
func (c *ClusterConfig) FindEnrollmentByID(id string) *PendingEnrollment {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.PendingEnrollments {
		if c.PendingEnrollments[i].ID == id {
			e := c.PendingEnrollments[i]
			cp := e
			if e.PendingJoin != nil {
				pj := *e.PendingJoin
				cp.PendingJoin = &pj
			}
			return &cp
		}
	}
	return nil
}

// QuorumSize returns the minimum number of live nodes required for
// the cluster to make progress: floor(N/2) + 1.
func (c *ClusterConfig) QuorumSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := len(c.Peers)
	if n == 0 {
		return 1
	}
	return n/2 + 1
}
