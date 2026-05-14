# qu — quorum-based uptime monitor

`qu` is a small Linux daemon that watches HTTP, TCP, and ICMP endpoints
from several cooperating nodes. The nodes form a quorum cluster; one is
elected master and owns alert dispatch. A check is only reported as
**DOWN** when the majority of nodes agree, which keeps a single node's
flaky uplink from paging anyone at 3am.

A single static binary contains the daemon, the CLI, and everything in
between. Inter-node traffic is mutual TLS with SSH-style fingerprint
trust — no central CA, no shared secret.

## Why

Most uptime monitors are either a SaaS or a single box that, by
definition, can't tell you when it's the one that's down. `qu` solves
both: run it on a few cheap hosts in different networks and they vote
on truth. If one of them loses its uplink, the rest keep alerting.

## Architecture

```
   +-------------- node A ---------------+
   | qu serve                            |
   | ├─ transport server (mTLS :9901)    |
   | ├─ quorum manager  (heartbeats)     |
   | ├─ replicator      (cluster.yaml)   |
   | ├─ scheduler       (HTTP/TCP/ICMP)  |  <─── probes targets
   | ├─ aggregator      (master-only)    |
   | ├─ alerts          (master-only)    |
   | └─ control socket  (unix, for CLI)  |
   +-------------------------------------+
              │  ▲   mTLS, pinned by fingerprint
              ▼  │
        node B   node C   …
```

Every node runs every probe. Results are shipped to the elected master,
which folds them into a per-check sliding window. A state flips (UP↔DOWN)
only after **two consecutive aggregate evaluations** agree — that's
the hysteresis that absorbs network blips.

Master election is deterministic: among the live members of the quorum,
the node with the lexicographically smallest NodeID wins. No
negotiation, no split-brain window.

`cluster.yaml` is the single replicated source of truth (peers, checks,
alerts). Mutations from the CLI route through the master, which bumps a
monotonic version and broadcasts the result. The same file is also
watched on disk, so an operator can `sudoedit cluster.yaml` on any node
and the daemon will replicate the edit cluster-wide.

## Build

Requires Go 1.23 or newer.

```sh
go build -o qu ./cmd/qu
```

To stamp the version into the binary:

```sh
go build -ldflags "-X main.version=v0.1.0" -o qu ./cmd/qu
qu --version
```

## Releases

Pushing a tag matching `v*` triggers `.gitea/workflows/release.yaml`,
which runs the test suite, cross-compiles static Linux binaries for
amd64 and arm64, and publishes them as a Gitea release with a
`SHA256SUMS` file alongside.

```sh
git tag v0.1.0
git push --tags
```

## Set up a 3-node cluster

On the **first host**:

```sh
qu init --advertise alpha.example.com:9901
```

That prints a random cluster secret. Copy it.

On every **other host**, pass that secret via `--secret`:

```sh
qu init --advertise bravo.example.com:9901  --secret <paste>
qu init --advertise charlie.example.com:9901 --secret <paste>
```

Without the matching secret a node cannot join, so random hosts that
can reach :9901 are safely ignored.

Start the daemon on every host (foreground; wire into systemd for prod):

```sh
qu serve
```

Then on one node — usually `alpha` — invite the others. The CLI prints
each remote's fingerprint and asks for confirmation SSH-style:

```sh
qu node add bravo.example.com:9901
qu node add charlie.example.com:9901
```

After the first invite, give it a few seconds for heartbeats to bring
the new peer into the live set before inviting the next one — otherwise
the local node's "needs ≥2 live to mutate" check will reject the
second add.

You only need to invite from one node. Peer certs ride along with the
replicated `cluster.yaml`, so every peer auto-trusts every other peer
without `N×(N-1)` invites.

That's it — the master broadcasts the new cluster config to every
trusting peer. `qu status` from any node should now show all three:

```
node       a7f3...
term       2
master     a7f3...
quorum     true (need 2)
config ver 4

PEERS
NODE_ID  ADVERTISE                 LIVE  LAST_SEEN
a7f3...  alpha.example.com:9901    true  2026-05-12T15:01:32Z
b21c...  bravo.example.com:9901    true  2026-05-12T15:01:32Z
c0d4...  charlie.example.com:9901  true  2026-05-12T15:01:32Z
```

## Adding checks and alerts

```sh
# alerts first so checks can reference them
qu alert add discord oncall --webhook https://discord.com/api/webhooks/...
qu alert add smtp   ops    --host smtp.example.com --port 587 \
                            --from monitor@example.com --to ops@example.com \
                            --user mailbot --password '****' --starttls=true

# checks
qu check add http  homepage https://example.com  --expect 200  --alerts oncall,ops
qu check add tcp   db       db.internal:5432     --interval 15s
qu check add icmp  gateway  10.0.0.1             --interval 5s
```

Mutations always route to the master, which bumps a monotonic version
and pushes the new `cluster.yaml` to every peer. If quorum is lost,
mutating commands fail loudly.

`qu status` shows the effective alert list for each check. Default
alerts are suffixed with `*` so you can tell at a glance which alerts
were attached automatically vs explicitly listed on the check:

```
CHECKS
ID        NAME      STATE  OK/TOTAL  ALERTS         DETAIL
ddbd...   homepage  up     3/3       oncall,ops*    
0006...   db        down   1/3       ops*           dial timeout
24f4...   gateway   up     3/3       -              
(alerts marked * are attached as defaults)
```

## Default alerts (attach to every check)

Rather than listing the same `--alerts` on every `check add`, mark an
alert as default and it fires for every check automatically:

```sh
# at creation
qu alert add discord oncall --webhook https://... --default

# or toggle later
qu alert default oncall on
qu alert default oncall off
```

`qu alert list` shows a DEFAULT column. A check can opt out of a
specific default by adding the alert's ID or name to its
`suppress_alert_ids` list in `cluster.yaml` (see "Edit cluster.yaml
directly" below).

## Interactive TUI

Prefer a dashboard over typing commands? `qu tui` opens a full-screen
[bubbletea](https://github.com/charmbracelet/bubbletea) UI over the
local daemon socket. The header shows quorum, master, term, and config
version; three tabs hold peers, checks, and alerts with auto-refresh
every two seconds.

```
┌─ QUptime ── node: 88a00af9   master: 3438fd6f   (follower)  ● quorum 3/2  term 4   ver 10 ──┐
│ Peers (3)   [2] Checks (3)   [3] Alerts (1)                                                  │
├──────────────────────────────────────────────────────────────────────────────────────────────┤
│ ID         NAME      STATE     OK/TOTAL  ALERTS    DETAIL                                    │
│ ddbd...    homepage  ● up      3/3       oncall*                                             │
│ 0006...    db        ● down    1/3       oncall*   dial timeout                              │
│ 24f4...    gateway   ○ unknown 0/0       -                                                   │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
↑↓ navigate   ⇥ next tab   1/2/3 jump   r refresh   a add check  d remove check   q quit
```

Keybindings:

| Key | Action |
|---|---|
| `↑` / `↓` | move cursor within a tab |
| `Tab` / `Shift+Tab` | next / previous tab |
| `1` / `2` / `3` | jump to Peers / Checks / Alerts |
| `r` | force-refresh |
| `a` | add (opens a picker on Checks/Alerts; node form on Peers) |
| `d` | remove the selected row (confirmation prompt) |
| `t` | send a test message to the selected alert |
| `D` | toggle the selected alert's `default` flag |
| `q` / `Ctrl+C` | quit |

Forms run the same control-plane methods the CLI does, so any side
effect (a mutation, a node add, an alert test) ends up routed through
the master exactly like `qu …` from the shell.

## Custom alert messages

Each alert can carry its own `subject_template` and `body_template`
(Go `text/template` syntax). When set, they override the built-in
formatting for that one alert; the default renderer is used otherwise.
Discord ignores the subject template (it has no subject line).

```sh
qu alert add discord oncall --webhook https://... \
    --body ':rotating_light: **{{.Check.Name}}** is now {{.Verb}}
target: `{{.Check.Target}}`
detail: {{.Snapshot.Detail}}'

# multi-line templates are easier from a file
qu alert add smtp ops --host ... --from ... --to ... \
    --subject-file /etc/quptime/templates/ops.subject \
    --body-file    /etc/quptime/templates/ops.body
```

Available template variables:

| Variable | Meaning |
|---|---|
| `{{.Check.Name}}` | check name |
| `{{.Check.Type}}` | `http` / `tcp` / `icmp` |
| `{{.Check.Target}}` | URL or host:port being probed |
| `{{.Check.ID}}` | UUID |
| `{{.From}}` | previous state (`up` / `down` / `unknown`) |
| `{{.To}}` | new state |
| `{{.Verb}}` | `UP` / `DOWN` / `RECOVERED` |
| `{{.Snapshot.Reports}}` | total per-node reports counted |
| `{{.Snapshot.OKCount}}` | how many reported OK |
| `{{.Snapshot.NotOK}}` | how many reported failure |
| `{{.Snapshot.Detail}}` | first failure detail string |
| `{{.NodeID}}` | master that dispatched |
| `{{.When}}` | RFC3339 timestamp |

`qu alert test <name>` exercises the template against a synthetic
"homepage going DOWN" transition, so you can verify rendering before
production traffic depends on it. A template parse or execution error
falls back to the built-in format and is logged.

## Edit cluster.yaml directly

Anything you can do through the CLI you can also do by editing
`$QUPTIME_DIR/cluster.yaml` on any node. The daemon polls the file every
few seconds; when it sees a hash that differs from what it last wrote,
it parses the YAML and forwards the change through the master, which
bumps the version and broadcasts the result everywhere — so a hand-edit
on `bravo` propagates to `alpha` and `charlie` automatically.

```sh
sudoedit /etc/quptime/cluster.yaml
# add `default: true` to an alert, or `suppress_alert_ids: [oncall]`
# on a check, then save and quit
```

You'll see a `manual-edit: cluster.yaml changed externally —
replicating via master` line in the daemon log when it picks the change
up. Invalid YAML is logged and ignored until you save a valid file.

The replicated fields are `peers`, `checks`, and `alerts`. `version`,
`updated_at`, and `updated_by` are server-controlled — the master
overwrites them on commit.

## Test an alert without waiting for a real outage

```sh
qu alert test oncall
```

## File layout

A node's state lives under `$QUPTIME_DIR` (defaults to `/etc/quptime`
when root, `~/.config/quptime` otherwise):

```
node.yaml      identity (NodeID, bind addr, port). Never replicated.
cluster.yaml   replicated state: peers, checks, alerts, version.
trust.yaml     local fingerprint trust store.
keys/          RSA private + public + self-signed cert.
```

The CLI talks to the local daemon over a unix socket at
`$QUPTIME_SOCKET` (defaults to `/var/run/quptime/quptime.sock` when
root, `$XDG_RUNTIME_DIR/quptime/quptime.sock` otherwise) — filesystem
permissions guard it; no TLS on the local socket.

## ICMP and capabilities

ICMP checks default to unprivileged UDP-mode pings so the daemon does
not need root or `CAP_NET_RAW`. If you want classic raw ICMP, either
run the daemon as root or grant the capability:

```sh
sudo setcap cap_net_raw=+ep ./qu
```

## CLI reference

```
qu init                                       generate identity + keys
qu serve                                      run the daemon
qu status                                     quorum, master, check states
qu tui                                        interactive dashboard
qu node add    <host:port>                    TOFU-add a peer
qu node list                                  show peers + liveness
qu node remove <node-id>                      remove from cluster + trust
qu check add http  <name> <url>  [--expect 200] [--interval 30s] [--body-match str] [--alerts a,b]
qu check add tcp   <name> <host:port>
qu check add icmp  <name> <host>
qu check list
qu check remove <id-or-name>
qu alert add smtp    <name> --host … --port … --from … --to … [--user --password --starttls] [--default] [--subject … --body …]
qu alert add discord <name> --webhook …                                                        [--default] [--body …]
qu alert list / remove / test <id-or-name>
qu alert default <id-or-name> on|off            toggle default attachment to every check
qu trust list / remove <node-id>
```

All `--interval` and `--timeout` flags accept Go duration syntax: `5s`,
`1m30s`, `2h`, etc.

## Tests

```sh
go test ./...
go test -race ./...
```

Each internal package has unit tests; coverage hovers around 60–90 %
on the meaningful packages. The transport tests bring up real mTLS
listeners over loopback, which exercises the cert pinning end-to-end.

## What's intentionally not here (v1)

- No web UI. The CLI is the only operator surface.
- No historical metrics or SLA reports — only the current aggregate
  state is kept in memory. Add SQLite later if you need graphs.
- No automatic key rotation. Re-init a node and re-trust if you need
  to roll its identity.
- No multi-tenant isolation. One cluster = one set of checks.

## Layout

```
cmd/qu/                    entry point
internal/config/           on-disk file layout, ClusterConfig, NodeConfig
internal/crypto/           RSA keypair + self-signed cert + SPKI fingerprints
internal/trust/            fingerprint trust store
internal/transport/        mTLS listener/dialer, framed JSON-RPC
internal/quorum/           heartbeats + deterministic master election
internal/replicate/        master-routed mutations, version-gated replication
internal/checks/           HTTP/TCP/ICMP probers, scheduler, aggregator
internal/alerts/           SMTP + Discord dispatchers, message rendering
internal/daemon/           glue: wires every component + control socket
internal/cli/              cobra commands, the user-facing surface
internal/tui/              bubbletea dashboard (qu tui)
```
