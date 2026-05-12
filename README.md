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
qu node add    <host:port>                    TOFU-add a peer
qu node list                                  show peers + liveness
qu node remove <node-id>                      remove from cluster + trust
qu check add http  <name> <url>  [--expect 200] [--interval 30s] [--body-match str] [--alerts a,b]
qu check add tcp   <name> <host:port>
qu check add icmp  <name> <host>
qu check list
qu check remove <id-or-name>
qu alert add smtp    <name> --host … --port … --from … --to … [--user --password --starttls]
qu alert add discord <name> --webhook …
qu alert list / remove / test <id-or-name>
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
```
