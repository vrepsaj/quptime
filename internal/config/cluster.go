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

	// AlertIDs lists which configured alerts fire when this check
	// transitions state.
	AlertIDs []string `yaml:"alert_ids,omitempty"`

	// SuppressAlertIDs lets a check opt out of specific default alerts.
	SuppressAlertIDs []string `yaml:"suppress_alert_ids,omitempty"`
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

// ClusterConfig is the replicated cluster state. The Version field
// strictly increases on every mutation; the master is the only node
// that bumps it.
type ClusterConfig struct {
	Version   uint64    `yaml:"version"`
	UpdatedAt time.Time `yaml:"updated_at"`
	UpdatedBy string    `yaml:"updated_by"`

	Peers  []PeerInfo `yaml:"peers"`
	Checks []Check    `yaml:"checks"`
	Alerts []Alert    `yaml:"alerts"`

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
		Version:   c.Version,
		UpdatedAt: c.UpdatedAt,
		UpdatedBy: c.UpdatedBy,
		Peers:     append([]PeerInfo(nil), c.Peers...),
		Checks:    append([]Check(nil), c.Checks...),
		Alerts:    append([]Alert(nil), c.Alerts...),
	}
	return cp
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
