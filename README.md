# qu — quorum-based uptime monitor

`qu` is a small Linux daemon that watches HTTP, TCP, and ICMP endpoints
from several cooperating nodes. The nodes form a quorum cluster; one is
elected master and owns alert dispatch. A check is only reported as
**DOWN** when the majority of nodes agree, which keeps a single node's
flaky uplink from paging anyone at 3am.

A single static binary contains the daemon, the CLI, and everything in
between. Inter-node traffic is mutual TLS with SSH-style fingerprint
trust — no central CA, no shared secret.

<details>
<summary>Table of contents</summary>

- [qu — quorum-based uptime monitor](#qu--quorum-based-uptime-monitor)
  - [Installation](#installation)
    - [From pre-built binary](#from-pre-built-binary)
    - [From Docker](#from-docker)
  - [Why](#why)
  - [Documentation](#documentation)
  - [Architecture](#architecture)
  - [Build](#build)
  - [Releases](#releases)
  - [Set up a 3-node cluster](#set-up-a-3-node-cluster)
  - [Adding checks and alerts](#adding-checks-and-alerts)
  - [Default alerts (attach to every check)](#default-alerts-attach-to-every-check)
  - [Pause checks and alerts without deleting them](#pause-checks-and-alerts-without-deleting-them)
  - [Bypass the host's DNS cache (custom resolvers)](#bypass-the-hosts-dns-cache-custom-resolvers)
  - [Interactive TUI](#interactive-tui)
  - [Custom alert messages](#custom-alert-messages)
    - [Conditionals, pipelines, and worked examples](#conditionals-pipelines-and-worked-examples)
  - [Edit cluster.yaml directly](#edit-clusteryaml-directly)
  - [Test an alert without waiting for a real outage](#test-an-alert-without-waiting-for-a-real-outage)
  - [File layout](#file-layout)
  - [ICMP and capabilities](#icmp-and-capabilities)
  - [CLI reference](#cli-reference)
  - [Tests](#tests)
  - [What's intentionally not here (v1)](#whats-intentionally-not-here-v1)
  - [Layout](#layout)

</details>

## Installation

### From pre-built binary

The canonical home is Gitea; the repo is push-mirrored to GitHub on
every tag. Releases and multi-arch container images are published to
both.

| Source          | Releases                                        | Container image                |
| --------------- | ----------------------------------------------- | ------------------------------ |
| Gitea (primary) | <https://git.cer.sh/axodouble/quptime/releases> | `git.cer.sh/axodouble/quptime` |
| GitHub (mirror) | <https://github.com/Axodouble/QUptime/releases> | `ghcr.io/axodouble/quptime`    |

One-step install — tries Gitea first, falls back to GitHub automatically:

```sh
curl -fsSL https://git.cer.sh/Axodouble/QUptime/raw/branch/master/install.sh | sudo bash
# or, via the GitHub mirror:
# curl -fsSL https://raw.githubusercontent.com/Axodouble/QUptime/master/install.sh | sudo bash
```

The script verifies the binary against the published `SHA256SUMS`
before installing and refuses to proceed on a mismatch.

### From Docker

```sh
docker pull git.cer.sh/axodouble/quptime:latest
# or, via the GitHub mirror:
# docker pull ghcr.io/axodouble/quptime:latest
```

See [docs/deployment/docker.md](docs/deployment/docker.md) for compose
recipes.

## Why

Most uptime monitors are either a SaaS or a single box that, by
definition, can't tell you when it's the one that's down. `qu` solves
both: run it on a few cheap hosts in different networks and they vote
on truth. If one of them loses its uplink, the rest keep alerting.

## Documentation

This README is the quick-start. For production use, the longer guides
live under [`docs/`](docs/README.md):

| If you want to…                                       | Read                                                                     |
| ----------------------------------------------------- | ------------------------------------------------------------------------ |
| understand the consensus / replication model          | [docs/architecture.md](docs/architecture.md)                             |
| reference every field in `node.yaml` / `cluster.yaml` | [docs/configuration.md](docs/configuration.md)                           |
| deploy on Linux with systemd hardening                | [docs/deployment/systemd.md](docs/deployment/systemd.md)                 |
| deploy with Docker / docker-compose                   | [docs/deployment/docker.md](docs/deployment/docker.md)                   |
| deploy over Tailscale or WireGuard                    | [docs/deployment/tailscale.md](docs/deployment/tailscale.md)             |
| expose `qu` on the open internet safely               | [docs/deployment/public-internet.md](docs/deployment/public-internet.md) |
| upgrade, back up, or recover from failures            | [docs/operations.md](docs/operations.md)                                 |
| understand the trust model and rotate identities      | [docs/security.md](docs/security.md)                                     |
| diagnose a misbehaving cluster                        | [docs/troubleshooting.md](docs/troubleshooting.md)                       |

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
negotiation, no split-brain window. A 2-minute **master cooldown**
keeps the current master in place until a returning lower-NodeID peer
has been continuously live for the full window, so a self-monitoring
master that briefly drops doesn't flap the role back the instant it
reappears.

`cluster.yaml` is the single replicated source of truth (peers, checks,
alerts). Mutations from the CLI route through the master, which bumps a
monotonic version and broadcasts the result. The same file is also
watched on disk, so an operator can `sudoedit cluster.yaml` on any node
and the daemon will replicate the edit cluster-wide.

## Build

Requires Go 1.24.2 or newer.

```sh
go build -o qu ./cmd/qu
```

To stamp the version into the binary:

```sh
go build -ldflags "-X main.version=v0.0.1" -o qu ./cmd/qu
qu --version
```

## Releases

Pushing a tag matching `v*` triggers `.gitea/workflows/release.yaml`,
which runs the test suite, cross-compiles static Linux binaries for
amd64 and arm64, and publishes them as a Gitea release with a
`SHA256SUMS` file alongside.

```sh
git tag v0.0.1
git push --tags
```

## Set up a 3-node cluster

On the **first host**, bootstrap and start the daemon:

```sh
qu init --advertise alpha.example.com:9901
qu serve
```

For every additional host, mint a pre-deployment enrollment token on
`alpha` and redeem it on the new host. Tokens are single-use,
time-limited, and pin the cluster's TLS fingerprint so the new host
can't be tricked into joining a different cluster.

```sh
# On alpha (the existing cluster):
qu enroll create --name bravo --auto-approve --ttl 1h
# → prints a single `qu enroll join <token>` command, copy it.

# On bravo (the new host, fresh data dir):
qu enroll join <paste> --advertise bravo.example.com:9901
qu serve
```

`--auto-approve` makes the cluster accept the joiner automatically on
submission — handy for cloud-init / Ansible. Drop the flag if you'd
rather approve interactively from the cluster:

```sh
# On bravo:
qu enroll join <token> --advertise bravo.example.com:9901
# → prints: "enrollment submitted; waiting for cluster-side approval"

# Back on alpha:
qu enroll list                # see pending submissions
qu enroll approve <token-id>  # commit; bravo becomes a peer
```

Either way, trust is acquired from both sides: the joiner verifies the
cluster's TLS fingerprint (pinned into the token at create time) and
the cluster verifies the joiner via the token's hashed secret. There
is no shared cluster-wide secret — see [docs/security.md](docs/security.md)
for the threat model.

Peer certs ride along with the replicated `cluster.yaml`, so every
peer auto-trusts every other peer without `N×(N-1)` enrollments.

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

> ⚠️ **Alert credentials are replicated cluster-wide.** SMTP passwords
> and Discord webhook URLs live in `cluster.yaml`, which is mirrored to
> every node. Any node that can read its own data directory can read
> every alert secret. Treat compromising one node as compromising every
> alert credential, and restrict who can reach `$QUPTIME_DIR` on each
> host (the hardened systemd unit and the Docker image both default to
> `0700`/`0750`). See [docs/security.md](docs/security.md) for the full
> threat model.

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
qu check add tls   cert     example.com          --warn-days 14
qu check add dns   apex     example.com          --record a --expect 93.184.
```

`tls` watches the leaf certificate's expiry — it flips DOWN when the
cert is expired or within `--warn-days` (default 14) of expiry. Chain
validity is intentionally not verified, so self-signed endpoints work
the same way. `dns` resolves the target (optionally against a specific
`--resolver`) and can require a substring in at least one answer via
`--expect`.

Mutations always route to the master, which bumps a monotonic version
and pushes the new `cluster.yaml` to every peer. If quorum is lost,
mutating commands fail loudly.

`qu status` shows the effective alert list for each check. Default
alerts are suffixed with `*` so you can tell at a glance which alerts
were attached automatically vs explicitly listed on the check:

```
CHECKS
ID        NAME      STATE             OK/TOTAL  ALERTS         DETAIL
ddbd...   homepage  up                3/3       oncall,ops*    
0006...   db        down              1/3       ops*           dial timeout
24f4...   gateway   up                3/3       -              
b8e2...   nightly   (disabled) up     0/0       ops*           
(alerts marked * are attached as defaults; "(disabled)" checks are paused — see `qu check enable`)
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

## Pause checks and alerts without deleting them

Both checks and alerts carry a `disabled` flag. A disabled check is
skipped by the scheduler (no probes are fired and no per-node results
arrive at the aggregator) and a disabled alert is filtered out of the
effective alert list (it neither fires on transitions nor counts as a
default attachment). Useful for planned maintenance, hush-during-a-known-outage, or temporarily silencing a noisy channel without
losing its configuration.

```sh
qu check disable homepage      # stop probing
qu check enable  homepage      # resume

qu alert disable oncall        # silence the channel
qu alert enable  oncall        # bring it back

qu check list                  # disabled checks show "(disabled) <state>"
qu alert list                  # ENABLED column shows true/false
```

Toggling is a regular cluster mutation: it routes through the master
and replicates like any other edit. In the TUI, `x` on the Checks or
Alerts tab toggles the selected row.

## Bypass the host's DNS cache (custom resolvers)

By default each probe resolves its target through the host's system
resolver — which means an `nscd` / `systemd-resolved` cache, or a
sleepy local DNS server, can keep a check pointed at an IP that has
since moved. To bypass that path, point `qu` at the resolvers you
trust:

```sh
# Cluster-wide default: every check that doesn't override uses these.
# Tried in order with connection-level failover.
qu cluster resolvers set 1.1.1.1 1.0.0.1
qu cluster resolvers show
qu cluster resolvers clear

# Per-check override (always wins over the cluster default):
qu check add http homepage https://example.com --resolvers 1.1.1.1,1.0.0.1
qu check edit homepage --resolvers 8.8.8.8,8.8.4.4
qu check edit homepage --resolvers ''   # clear; fall back to cluster default
```

The resolver list applies to HTTP / TCP / TLS / ICMP target lookups
and (for DNS checks) to the query itself. Each entry is a
`host[:port]`; a bare host gets `:53` appended at use time. Literal
IP targets skip the resolver entirely — there's nothing to look up.

Precedence on every probe is **check → cluster → legacy DNSResolver
(DNS checks only) → host system resolver**.

## Interactive TUI

Prefer a dashboard over typing commands? `qu tui` opens a full-screen
[Bubble Tea v2](https://charm.land/bubbletea) UI over the local
daemon socket. The header shows quorum, master, term, and config
version; three tabs hold peers, checks, and alerts with auto-refresh
every two seconds.

```
┌─ QUptime ── node: 88a00af9   master: 3438fd6f   (follower)  ● quorum 3/2  term 4   ver 10 ──┐
│ Peers (3)   [2] Checks (3)   [3] Alerts (1)                                                  │
├──────────────────────────────────────────────────────────────────────────────────────────────┤
│ ID         NAME      ON   STATE     OK/TOTAL  ALERTS    DETAIL                               │
│ ddbd...    homepage  yes  ● up      3/3       oncall*                                        │
│ 0006...    db        yes  ● down    1/3       oncall*   dial timeout                         │
│ 24f4...    gateway   no   ○ unknown 0/0       -                                              │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
↑↓ navigate   ⇥ next tab   1/2/3 jump   r refresh   a add  d remove  e edit  t test  x on/off  q quit
```

Keybindings:

| Key                 | Action                                                                                     |
| ------------------- | ------------------------------------------------------------------------------------------ |
| `↑` / `↓`           | move cursor within a tab                                                                   |
| `Tab` / `Shift+Tab` | next / previous tab                                                                        |
| `1` / `2` / `3`     | jump to Peers / Checks / Alerts                                                            |
| `r`                 | force-refresh                                                                              |
| `a`                 | add (opens a picker on Checks/Alerts; node form on Peers)                                  |
| `d`                 | remove the selected row (confirmation prompt)                                              |
| `t`                 | fire a test transition: synthetic test message on Alerts; pick down/up/recovered on Checks |
| `x`                 | toggle the selected check / alert on or off (pauses the row without deleting it)           |
| `D`                 | toggle the selected alert's `default` flag                                                 |
| `q` / `Ctrl+C`      | quit                                                                                       |

Forms run the same control-plane methods the CLI does, so any side
effect (a mutation, a node add, an alert test) ends up routed through
the master exactly like `qu …` from the shell.

## Custom alert messages

Each alert can carry its own `subject_template` and `body_template`
(Go `text/template` syntax). When set, they override the built-in
formatting for that one alert; the default renderer is used otherwise.
Discord ignores the subject template (it has no subject line).

The built-in renderer picks a different template per check type — HTTP
surfaces the URL and expected status, TLS surfaces the certificate
state and warn window, DNS surfaces the record / resolver / expected
substring, and TCP / ICMP keep a minimal connectivity-focused shape.
The literal template sources live in
[`docs/configuration.md`](docs/configuration.md#default-alert-templates-per-check-type)
under "Default alert templates"; paste any of them into an alert as a
starting point for customisation.

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

| Variable                | Meaning                                      |
| ----------------------- | -------------------------------------------- |
| `{{.Check.Name}}`       | check name                                   |
| `{{.Check.Type}}`       | `http` / `tcp` / `icmp`                      |
| `{{.Check.Target}}`     | URL or host:port being probed                |
| `{{.Check.ID}}`         | UUID                                         |
| `{{.From}}`             | previous state (`up` / `down` / `unknown`)   |
| `{{.To}}`               | new state                                    |
| `{{.Verb}}`             | `UP` / `DOWN` / `RECOVERED` (see note below) |
| `{{.VerbLower}}`        | lowercase form (`up` / `down` / `recovered`) |
| `{{.Snapshot.Reports}}` | total per-node reports counted               |
| `{{.Snapshot.OKCount}}` | how many reported OK                         |
| `{{.Snapshot.NotOK}}`   | how many reported failure                    |
| `{{.Snapshot.Detail}}`  | first failure detail string                  |
| `{{.NodeID}}`           | master that dispatched                       |
| `{{.When}}`             | RFC3339 timestamp                            |

**When does `UP` fire vs. `RECOVERED`?** Every check starts in the
`unknown` state. The first time the cluster agrees a check is healthy,
`unknown → up` fires with `Verb = UP` — this is the "we've never seen
this check work before" announcement, typically only at first startup
or right after a check is added. Once the check has ever been `down`,
the recovery transition is `down → up` and fires with `Verb =
RECOVERED` instead. So in normal day-to-day operation you'll see
`DOWN` and `RECOVERED` pairs; `UP` is the one-shot initial-health
notice and is not re-emitted when a service comes back from an
outage.

The same variable list is surfaced in-app: `qu alert add smtp --help`,
`qu alert add discord --help`, and `qu alert edit --help` each print
it under their flag table, and `qu tui` shows a compact reminder of
the supported variables as a hint when the cursor lands on a Subject
or Body template field in the add/edit alert forms.

`qu alert test <name>` exercises the template against a synthetic
"homepage going DOWN" transition, so you can verify rendering before
production traffic depends on it. A template parse or execution error
falls back to the built-in format and is logged.

`qu check test <name> [--state down|up|recovered]` goes one step
further: it fires a synthetic transition for a *real* check through
**every** alert that would actually receive it (defaults plus the
check's explicit `alert_ids`, minus `suppress_alert_ids`). The
`Detail` is a type-aware placeholder — e.g. a TLS check renders as
"cert expires in 7d", a DNS check as "lookup …: no such host" — so
you can preview what your templates will look like for each probe
type without waiting for a real outage. The hysteresis filter that
normally suppresses Unknown→Up is bypassed for tests, so all three
verbs (DOWN, RECOVERED, UP) actually fire. In the TUI, hit `t` on
the Checks tab to get a picker for the same three transitions.

### Conditionals, pipelines, and worked examples

Templates use Go's `text/template` syntax, so you have `if`/`else if`/
`else`/`end`, comparison helpers (`eq`, `ne`, `lt`, `gt`), `printf`
pipelines, and `with` blocks. The default rendering — the one used
when no custom template is set — lives in `internal/alerts/message.go`
inside the `Render` function; tweak it there if you want to change
what every alert without an override produces.

A few progressively richer examples:

**1. State-specific Discord copy** — different tone for `DOWN`,
`RECOVERED`, and first-time `UP`:

```yaml
body_template: |
  {{if eq .Verb "DOWN"}}:rotating_light: **{{.Check.Name}}** is DOWN
  We're investigating. Last detail: `{{.Snapshot.Detail}}`
  {{else if eq .Verb "RECOVERED"}}:white_check_mark: **{{.Check.Name}}** is back UP after a {{.From}} blip.
  {{else}}:information_source: **{{.Check.Name}}** is online ({{.VerbLower}}).{{end}}
```

**2. SMTP subject with severity prefix and run-length detail** —
pipes `Verb` through `printf` for padding and only mentions the
report count when it actually matters:

```yaml
subject_template: '[{{printf "%-9s" .Verb}}] {{.Check.Name}} — {{.Check.Target}}'
body_template: |
  Check:    {{.Check.Name}} ({{.Check.Type}})
  Target:   {{.Check.Target}}
  Status:   {{.Verb}} (was {{.From}})
  Reporter: {{.NodeID}}
  At:       {{.When}}
  {{if gt .Snapshot.Reports 1}}
  Quorum:   {{.Snapshot.OKCount}} ok / {{.Snapshot.NotOK}} failing across {{.Snapshot.Reports}} reports.
  {{end}}{{with .Snapshot.Detail}}
  Detail:   {{.}}
  {{end}}
```

**3. PagerDuty-style severity routing** — nest `if`/`else if` so a
single template can produce three different first lines without
duplicating the rest of the body:

```yaml
subject_template: >-
  {{if eq .Verb "DOWN"}}P1: {{.Check.Name}} hard down
  {{else if eq .Verb "RECOVERED"}}P3: {{.Check.Name}} recovered
  {{else}}P4: {{.Check.Name}} {{.VerbLower}}{{end}}
body_template: |
  {{/* Header line — uses .VerbLower so the prose reads naturally */}}
  {{.Check.Name}} ({{.Check.Target}}) is now {{.VerbLower}}.

  {{if eq .Verb "DOWN"-}}
  This is a real outage. Quorum: {{.Snapshot.NotOK}}/{{.Snapshot.Reports}} reporters see it failing.
  Detail from the first failing probe: {{.Snapshot.Detail}}
  Acknowledge in the runbook before paging on-call.
  {{- else if eq .Verb "RECOVERED" -}}
  Recovered after a {{.From}} period. No action needed; this is informational.
  {{- else -}}
  First successful probe after {{.From}}. Marking healthy.
  {{- end}}

  — {{.NodeID}} at {{.When}}
```

The `{{-` / `-}}` trim adjacent whitespace, which keeps the rendered
output tidy even when the template itself is indented for readability.

If a template fails to parse or panics at execute time, the
dispatcher falls back to the default `Render` output for that field
and logs the error — your alert still ships, you just lose the
custom formatting until you fix the template.

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
qu init                                       generate identity + keys (first node only)
qu serve                                      run the daemon
qu status                                     quorum, master, check states
qu tui                                        interactive dashboard
qu enroll create [--name …] [--ttl 1h] [--auto-approve]  mint a pre-deployment token
qu enroll list                                show outstanding tokens + pending approvals
qu enroll approve <id-or-name>                approve a pending enrollment
qu enroll revoke  <id-or-name>                revoke an outstanding token
qu enroll join    <token> [--advertise …]     redeem a token on a new host
qu node list                                  show peers + liveness
qu node remove <node-id>                      remove from cluster + trust
qu check add http  <name> <url>  [--expect 200] [--interval 30s] [--body-match str] [--alerts a,b] [--resolvers 1.1.1.1,1.0.0.1]
qu check add tcp   <name> <host:port>                                                                  [--resolvers 1.1.1.1,1.0.0.1]
qu check add icmp  <name> <host>                                                                       [--resolvers 1.1.1.1,1.0.0.1]
qu check add tls   <name> <host[:port]>          [--warn-days 14] [--sni name]                         [--resolvers 1.1.1.1,1.0.0.1]
qu check add dns   <name> <hostname>             [--record a|aaaa|cname|mx|txt|ns] [--resolver host:port] [--expect substr] [--resolvers …]
qu check list
qu check remove  <id-or-name>
qu check enable  <id-or-name>                   resume probing a previously disabled check
qu check disable <id-or-name>                   stop probing without deleting the check
qu check test    <id-or-name> [--state down|up|recovered]  fire a synthetic transition to exercise alert templates
qu alert add smtp    <name> --host … --port … --from … --to … [--user --password --starttls] [--default] [--subject … --body …]
qu alert add discord <name> --webhook …                                                        [--default] [--body …]
qu alert list / remove / test <id-or-name>
qu alert enable  <id-or-name>                   resume firing a previously disabled alert
qu alert disable <id-or-name>                   silence an alert without deleting it
qu alert default <id-or-name> on|off            toggle default attachment to every check
qu cluster resolvers show                       print the cluster-wide default DNS resolver list
qu cluster resolvers set  <r1> [<r2> …]         replace the cluster-wide resolver list (failover order)
qu cluster resolvers clear                      drop the cluster-wide list; every check falls back to host resolver
qu trust list / remove <node-id>
qu update [--check] [--force] [--source gitea|github] [--beta]   replace this binary with the latest release
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
internal/tui/              Bubble Tea v2 dashboard (qu tui)
```
