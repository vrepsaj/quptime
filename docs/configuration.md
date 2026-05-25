# Configuration

This page is the canonical reference for the on-disk files, the
environment variables, and every field that `qu` reads. It's
deliberately tedious — when something doesn't behave the way you
expect, this is where the answer lives.

## File layout

When running as **root** (the typical case under systemd):

```
/etc/quptime/
├── node.yaml          identity, never replicated
├── cluster.yaml       replicated state
├── trust.yaml         local fingerprint trust store
└── keys/
    ├── private.pem    RSA private key (0600)
    ├── public.pem     RSA public key
    └── cert.pem       self-signed X.509 cert

/var/run/quptime/quptime.sock   control socket (0600)
```

When running as a **non-root** user (the typical case for `go run` or a
desktop test):

```
~/.config/quptime/...                       same shape as /etc/quptime
$XDG_RUNTIME_DIR/quptime/quptime.sock       control socket
```

Override the data directory with `QUPTIME_DIR=/some/path qu serve`.
Override the socket path with `QUPTIME_SOCKET=/run/foo.sock`.

## Environment variables

### Paths

| Variable          | Purpose                                                                                                                   |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `QUPTIME_DIR`     | Data directory. Defaults to `/etc/quptime` (root) or `$XDG_CONFIG_HOME/quptime`.                                          |
| `QUPTIME_SOCKET`  | Path to the CLI ↔ daemon unix socket. Defaults to `/var/run/quptime/quptime.sock` (root) or `$XDG_RUNTIME_DIR/quptime/…`. |
| `XDG_CONFIG_HOME` | Honored when running as non-root and `QUPTIME_DIR` is unset.                                                              |
| `XDG_RUNTIME_DIR` | Honored when running as non-root and `QUPTIME_SOCKET` is unset.                                                           |

### `node.yaml` field overrides

Every field in `node.yaml` can also be supplied via an environment
variable. This is the recommended way to drive Docker / Compose
deployments: drop the env vars into the compose file and the daemon
will bootstrap on first start without a separate `qu init` step.

| Variable                 | `node.yaml` field | Notes                                                                                                          |
| ------------------------ | ----------------- | -------------------------------------------------------------------------------------------------------------- |
| `QUPTIME_NODE_ID`        | `node_id`         | Pin a specific UUID. Leave unset to let `qu init` / auto-init generate one.                                    |
| `QUPTIME_BIND_ADDR`      | `bind_addr`       | Defaults to `0.0.0.0`.                                                                                         |
| `QUPTIME_BIND_PORT`      | `bind_port`       | Integer. Defaults to `9901`.                                                                                   |
| `QUPTIME_ADVERTISE`      | `advertise`       | `host:port` other peers use to reach this node. Required when bound to a wildcard or behind NAT.               |
| `QUPTIME_CLUSTER_SECRET` | `cluster_secret`  | **Deprecated, ignored.** Kept declared so older compose files don't error on validation. Pre-deployment enrollment tokens replaced it; see [security.md](security.md). |

Precedence is **env > file > compiled default**. Non-empty env values
win over whatever is stored in `node.yaml` at load time, so changing a
variable in `docker-compose.yml` and restarting the container is
enough to roll out new bind/advertise values — no on-disk edit
required. Empty env values are ignored (they will not clear a
previously persisted field).

For `qu init` specifically, explicit command-line flags take
precedence over env values; env values fill in only the fields the
operator did not pass on the command line.

The daemon does not read any other environment variables. SMTP, Discord,
and HTTP probe targets are configured exclusively in `cluster.yaml`.

## Auto-init on `qu serve`

If `node.yaml` does not exist when `qu serve` starts, the daemon
bootstraps it in-place using the `QUPTIME_*` env vars above: a fresh
UUID is generated (or `QUPTIME_NODE_ID` is honored if set), an RSA
keypair and self-signed cert are written under `keys/`, and
`cluster.yaml` is seeded with this node as its sole peer. The host
joins an existing cluster only after an operator redeems a pre-deployment
token with `qu enroll join <token>`; serving with no enrollment leaves
the node as a one-member cluster of its own.

This is what makes the docker-compose flow `docker compose up`-only
on a fresh volume. To opt out (e.g. so a misconfigured deployment
crashes loudly instead of silently generating a new identity), run
`qu init` against the volume yourself before letting `qu serve` ever
see it.

## `node.yaml` — local identity

Never replicated. One file per host. Generated by `qu init`.

```yaml
node_id: 7f3a5b9e-...        # UUIDv4, immutable after init
bind_addr: 0.0.0.0           # listen address for :9901
bind_port: 9901              # listen port
advertise: alpha.example.com:9901   # how peers reach us; may differ from bind
# cluster_secret is no longer used. If present, the daemon clears it
# on the next start. New nodes join via `qu enroll join <token>`.
```

### Field reference

- `node_id` — UUIDv4 generated at `qu init`. Used by every peer to
  refer to this node across IP changes and restarts. Do not edit.
- `bind_addr` — Address the daemon listens on. `0.0.0.0` is the
  default. Set to `127.0.0.1` if you only want to expose the daemon
  through an overlay (Tailscale, WireGuard) — see
  [deployment/tailscale.md](deployment/tailscale.md).
- `bind_port` — Defaults to `9901`. Change here if 9901 is taken; the
  cluster does not require port-uniformity, peers just need to know
  what to dial via the `advertise` field.
- `advertise` — Host:port other nodes use to reach this one. Must be
  routable from every peer. Falls back to `bind_addr:bind_port` if
  unset, which is rarely what you want behind NAT.
- `cluster_secret` — **Deprecated.** Vestigial field from the pre-1.0
  shared-secret join model. The daemon does not consult it and blanks
  it on first start. New nodes enrol via single-use tokens; see
  [security.md](security.md).

### How `qu init` populates this file

```sh
qu init \
  --advertise alpha.example.com:9901 \
  --bind 0.0.0.0 \
  --port 9901
```

`qu init` is only for the *first* node of a brand-new cluster. To
add a second host, run `qu enroll create` on the first node and
`qu enroll join <token>` on the new one — it does the equivalent init
*and* submits enrollment in one step.

Idempotent in one direction only: if `node.yaml` exists, `qu init`
refuses to overwrite. To re-init, delete the data directory entirely.

## `cluster.yaml` — replicated state

This is the file that every node converges on. The master is the only
one allowed to bump `version`; followers `Replace` it whole each time
they receive a higher-versioned snapshot.

```yaml
version: 12
updated_at: 2026-05-15T14:01:00Z
updated_by: 7f3a5b9e-...
resolvers:                          # cluster-wide default DNS servers
  - 1.1.1.1                         # tried in order with failover
  - 1.0.0.1                         # omit / empty list to use each host's resolver
peers:
  - node_id: 7f3a5b9e-...
    advertise: alpha.example.com:9901
    fingerprint: SHA256:abcd...
    cert_pem: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
checks:
  - id: 0006a1...
    name: homepage
    type: http
    target: https://example.com
    interval: 30s
    timeout: 10s
    expect_status: 200
    alert_ids: [oncall]
    suppress_alert_ids: []
alerts:
  - id: f001ab...
    name: oncall
    type: discord
    default: true
    discord_webhook: https://discord.com/api/webhooks/...
    body_template: |
      :rotating_light: {{.Check.Name}} is {{.Verb}}
```

### Top-level fields

| Field        | Owner    | Notes                                                                              |
| ------------ | -------- | ---------------------------------------------------------------------------------- |
| `version`    | master   | Monotonic. Followers reject snapshots whose version is ≤ their local.              |
| `updated_at` | master   | UTC RFC3339. Cosmetic — humans use it, no logic depends on it.                     |
| `updated_by` | master   | NodeID of the committing master.                                                   |
| `peers`      | editable | Cluster members. Edits go through `add_peer` / `remove_peer` mutations.            |
| `checks`     | editable | Monitored targets.                                                                 |
| `alerts`     | editable | Notifier destinations.                                                             |
| `resolvers`  | editable | Cluster-wide default DNS-server list; used by checks with no `resolvers` of their own. Edit via `qu cluster resolvers set/clear`. |

### `peers[]`

```yaml
- node_id: 7f3a5b9e-...        # immutable, the peer's own UUID
  advertise: host:port         # how anyone dials this peer
  fingerprint: SHA256:...      # SPKI fingerprint of the peer's cert
  cert_pem: |                  # full PEM so other peers can mTLS without a separate invite
    -----BEGIN CERTIFICATE-----
    ...
```

The `cert_pem` field is what enables N-node clusters without N×(N-1)
manual invites: when peer X is added via the master, every other node
that receives the new `cluster.yaml` learns X's cert at the same time
and adds it to the local trust store. See
`internal/daemon/daemon.go:syncTrustFromCluster`.

### `checks[]`

```yaml
- id: 0006a1...           # UUIDv4, generated when the check is created
  name: homepage          # human-friendly, must be unique within cluster
  type: http              # http | tcp | icmp | tls | dns
  target: https://example.com
  interval: 30s           # Go duration syntax: 5s, 1m30s, 2h
  timeout: 10s            # default 10s
  expect_status: 200      # http only; 0 = accept anything < 400
  body_match: "OK"        # http only; substring match on response body
  tls_warn_days: 14       # tls only; trip when cert expires within N days
  tls_server_name: ""     # tls only; override SNI (default: host from target)
  dns_record: a           # dns only; a|aaaa|cname|mx|txt|ns (default a)
  dns_resolver: ""        # dns only; resolver host:port (default: system)
  dns_expect: ""          # dns only; substring required in an answer
  alert_ids: [oncall]     # alerts attached explicitly
  suppress_alert_ids: []  # opt out of specific default alerts
  disabled: false         # when true, the scheduler skips probing this check
  resolvers:              # optional per-check DNS-server list (host[:port])
    - 1.1.1.1             # tried in order with connection-level failover
    - 1.0.0.1             # empty = use the cluster default, then host resolver
```

Defaults:

- `interval`: 30s
- `timeout`: 10s
- `expect_status`: 0 → any 2xx is OK; otherwise the configured status
  must match exactly.
- `tls_warn_days`: 14
- `dns_record`: `a`
- `disabled`: `false` (omitted from `cluster.yaml` when false). When
  `true`, the scheduler stops probing the check — its worker is
  cancelled on the next reconcile pass and its existing per-node
  results age out of the aggregator without triggering a transition.
  Toggle from the CLI with `qu check enable|disable <id-or-name>` or
  from the TUI with `x` on the Checks tab.
- `resolvers`: `[]` (omitted from `cluster.yaml` when empty). When
  non-empty, the listed DNS servers are used to resolve the check's
  target instead of the host's stub resolver. Applies to HTTP / TCP /
  TLS / ICMP target lookups and to the DNS check's query itself. Each
  entry is a `host[:port]`; bare hosts get `:53` appended at use
  time. The list is walked in order with connection-level failover —
  `[1.1.1.1, 1.0.0.1]` falls through to Cloudflare's secondary if the
  primary is unreachable. Resolution of literal IP targets short-
  circuits and skips the resolver entirely. Empty means: use the
  cluster-wide default in `cluster.yaml.resolvers`; if that is also
  empty, the host's system resolver is used. For DNS checks,
  `dns_resolver` is honored as a legacy single-entry fallback when
  both lists are empty. Toggle from the CLI with `qu check edit
  --resolvers …` or set the cluster-wide default with `qu cluster
  resolvers set …`.

ICMP checks default to **unprivileged UDP-mode pings** so the daemon
does not need root. For raw ICMP, grant the capability — see
[deployment/systemd.md](deployment/systemd.md).

### DNS resolver precedence

Every probe (HTTP / TCP / TLS / ICMP / DNS) needs to translate its
target hostname into an IP at some point. The list it uses is picked
on each probe in this order:

1. `check.resolvers` — the per-check override.
2. `cluster.resolvers` — the cluster-wide default in `cluster.yaml`.
3. For **DNS-type checks only**, the legacy `check.dns_resolver`
   single-value field is honoured if it's set and both lists above
   are empty (kept for back-compat with configs written before
   `resolvers` existed).
4. The host's system resolver (`net.DefaultResolver` — `nscd`,
   `systemd-resolved`, `/etc/resolv.conf`, depending on platform).

Within a non-empty list, entries are tried in order with **connection-
level failover**: when the resolver dials, it walks the list and uses
the first server that accepts a connection. Subsequent queries reuse
the resolver, so query-level retries (e.g. `SERVFAIL`) do not roll
over to the next server in the list — only connectivity failures do.
This handles the realistic failure mode ("primary resolver is down")
without adding application-level retry on every lookup.

Literal IP targets (`10.0.0.1`, `https://192.0.2.1/`, etc.) skip the
resolver entirely — there is nothing to look up. ICMP only consults
the resolver list when there is at least one override configured; if
both check and cluster lists are empty, the underlying ping library
does its own lookup against the system resolver as before, so
existing ICMP checks behave unchanged.

TLS checks dial the target over TLS and inspect the leaf certificate's
`NotAfter`. Chain validity is intentionally **not** verified (self-signed
targets are a legitimate use case); the check fires when the cert is
expired or within `tls_warn_days` of expiry. Target may be a bare host,
`host:port`, or a full `https://` URL — bare hosts default to port 443.

DNS checks resolve the target via the configured resolver (or the
system resolver if none). Empty answer sets fail. When `dns_expect` is
set, at least one answer must contain that substring (case-insensitive)
for the check to be UP.

### `alerts[]`

Two notifier kinds, distinguished by `type`:

```yaml
# Discord
- id: f001ab...
  name: oncall
  type: discord
  default: true              # attach to every check automatically
  disabled: false            # when true, the dispatcher drops this alert entirely
  discord_webhook: https://...
  body_template: |           # optional Go text/template override
    {{.Check.Name}} is {{.Verb}}

# SMTP
- id: f002cd...
  name: ops
  type: smtp
  smtp_host: smtp.example.com
  smtp_port: 587
  smtp_user: mailbot
  smtp_password: '...'
  smtp_from: monitor@example.com
  smtp_to: [ops@example.com]
  smtp_starttls: true
  subject_template: '[{{.Verb}}] {{.Check.Name}}'
  body_template: |
    Check {{.Check.Name}} ({{.Check.Target}}) is now {{.Verb}}.
```

The `disabled` field is `omitempty` and defaults to `false`. When
`true`, `EffectiveAlertsFor` filters the alert out before the
dispatcher sees it — it does not fire on transitions and is dropped
from the default-attach set, regardless of `default: true`. Toggle
from the CLI with `qu alert enable|disable <id-or-name>` or from the
TUI with `x` on the Alerts tab.

If `default: true`, the alert fires for every check unless the check
lists the alert's ID or name in `suppress_alert_ids`. Otherwise the
alert only fires for checks that name it in `alert_ids`.

Templates are Go `text/template`. The full variable list is in the
top-level README under "Custom alert messages" — `qu alert add smtp
--help` and `qu alert add discord --help` print the same table.

### Default alert templates (per check type)

When `subject_template` / `body_template` are left empty, the daemon
renders the message with a built-in template chosen by the check's
`type`. Each one surfaces the fields that matter for that probe — HTTP
shows the URL and expected status, TLS shows the cert state and warn
window, DNS shows the record / resolver / expected substring, etc.

The templates below are the literal source of the built-ins (see
`internal/alerts/defaults.go`). Copy any of them into an alert's
`subject_template` / `body_template` as a starting point for
customisation; tweak the wording, drop fields you don't care about,
or wrap sections in `{{if …}}` blocks.

#### HTTP

```
[quptime] HTTP {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})
```

```
HTTP endpoint "{{.Check.Name}}" is now {{.VerbLower}}.

URL:        {{.Check.Target}}
{{- if .Check.ExpectStatus}}
Expected:   HTTP {{.Check.ExpectStatus}}
{{- end}}
{{- if .Check.BodyMatch}}
Body match: contains "{{.Check.BodyMatch}}"
{{- end}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
```

#### TLS

```
[quptime] TLS cert {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})
```

```
TLS certificate for "{{.Check.Name}}" is now {{.VerbLower}}.

Host:        {{.Check.Target}}
{{- if .Check.TLSServerName}}
SNI:         {{.Check.TLSServerName}}
{{- end}}
{{- if .Check.TLSWarnDays}}
Warn window: {{.Check.TLSWarnDays}}d before NotAfter
{{- end}}
{{- if .Snapshot.Detail}}
Cert state:  {{.Snapshot.Detail}}
{{- end}}
Previous:    {{.From}}
Reporters:   {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:      {{.NodeID}}
When:        {{.When}}
```

#### TCP

```
[quptime] TCP {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})
```

```
TCP service "{{.Check.Name}}" is now {{.VerbLower}}.

Endpoint:   {{.Check.Target}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
```

#### ICMP

```
[quptime] Ping {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})
```

```
Host "{{.Check.Name}}" is now {{.VerbLower}}.

Host:       {{.Check.Target}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
```

#### DNS

```
[quptime] DNS {{.Verb}} — {{.Check.Name}} ({{.Check.Target}})
```

```
DNS lookup for "{{.Check.Name}}" is now {{.VerbLower}}.

Target:     {{.Check.Target}}
{{- if .Check.DNSRecord}}
Record:     {{.Check.DNSRecord}}
{{- end}}
{{- if .Check.DNSResolver}}
Resolver:   {{.Check.DNSResolver}}
{{- end}}
{{- if .Check.DNSExpect}}
Expected:   contains "{{.Check.DNSExpect}}"
{{- end}}
{{- if .Snapshot.Detail}}
Detail:     {{.Snapshot.Detail}}
{{- end}}
Previous:   {{.From}}
Reporters:  {{.Snapshot.OKCount}}/{{.Snapshot.Reports}} OK, {{.Snapshot.NotOK}} failing
Master:     {{.NodeID}}
When:       {{.When}}
```

A future check type without a dedicated template falls back to a
generic version that prints `Target` plus the `Type` tag — see the
`DefaultBodyGeneric` constant in `internal/alerts/defaults.go`.

### Suppression precedence

For each check, the dispatcher computes the effective alert list as:

```
( explicit alert_ids ∪ alerts with default=true ) \ suppress_alert_ids \ disabled alerts
```

de-duplicated by alert ID. So a check can both opt in to specific
alerts and opt out of specific defaults; alerts with `disabled: true`
are removed unconditionally and never appear in any check's effective
list. A check with `disabled: true` is never probed, so its
aggregator state goes stale and no transitions are ever computed for
it — the alert list above is moot for disabled checks.

## `trust.yaml` — local trust store

A flat list of fingerprints this node accepts. One entry per peer,
populated by `qu node add` (or pulled in automatically when a peer's
cert arrives via the replicated `cluster.yaml`).

```yaml
entries:
  - node_id: 7f3a5b9e-...
    address: alpha.example.com:9901
    fingerprint: SHA256:...
    cert_pem: |
      -----BEGIN CERTIFICATE-----
      ...
```

Never edit this by hand. Use `qu trust list` and `qu trust remove`.

## Key material

`keys/private.pem` is the only long-lived secret on disk (enrollment
tokens are minted on demand and short-lived). It's chmod 0600 by
default; preserve that. The public cert at `keys/cert.pem` is what
gets fingerprinted and shipped in `cluster.yaml.peers[].cert_pem`.

There is **no automatic key rotation**. Rolling a node's identity
means wiping its data directory, running `qu init` again, and
re-adding it from another node as a fresh peer.

## Tunables that don't live in YAML

A few values are compiled constants. Change them in source and rebuild
if you need different behaviour.

| Constant                                              | Default | What it does                                                  |
| ----------------------------------------------------- | ------- | ------------------------------------------------------------- |
| `quorum.DefaultHeartbeatInterval`                     | `1s`    | How often each node heartbeats every peer.                    |
| `quorum.DefaultDeadAfter`                             | `4s`    | A peer is dead if no heartbeat is seen within this window.    |
| `checks.HysteresisCount`                              | `2`     | Consecutive aggregate evaluations needed before a state flip. |
| `checks.ReconcileInterval`                            | `5s`    | How often the scheduler reconciles its workers vs `checks[]`. |
| `daemon.manualEditPollInterval` (`internal/daemon/watcher.go`) | `2s`    | How often the daemon hashes `cluster.yaml` for hand edits.    |
