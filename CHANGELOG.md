# Changelog

All notable changes to this project are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.0.2] — 2026-05-15

### Fixed

- Text template field in the TUI did not support newlines, causing multi-line templates to render as a single line and losing formatting. This has been fixed by changing the field into a textarea and escaping the `enter` key to insert newlines.

## [v0.0.1] — 2026-05-15

Initial public release.

### Added

- **Quorum-based uptime monitoring.** Multiple cooperating nodes run
  the same probes (HTTP, TCP, ICMP) and vote on the cluster-wide
  truth. A check flips state only after two consecutive aggregate
  evaluations agree (hysteresis), so single-node flake doesn't page
  anyone.
- **Deterministic master election.** Among the live members of the
  quorum the lexicographically smallest NodeID wins — no negotiation
  step, no split-brain window.
- **mTLS inter-node transport** with TLS 1.3 minimum, SSH-style
  fingerprint pinning, and a pre-shared `cluster_secret` gating the
  Join RPC.
- **Replicated `cluster.yaml`** carrying peers, checks, and alerts.
  Master is the only writer; followers receive monotonic-versioned
  snapshots and converge on the latest. Hand-edits to the file on any
  node are picked up by the manual-edit watcher and forwarded through
  the master.
- **HTTP, TCP, and ICMP probes** with configurable interval,
  timeout, expected status, and optional body-substring match. ICMP
  defaults to unprivileged UDP-mode pings so the daemon can run as a
  non-root user.
- **SMTP and Discord alerts** with optional Go `text/template`
  subject/body overrides per alert, default-attach mode (`default:
  true`), and per-check opt-outs via `suppress_alert_ids`.
- **Docker-friendly env-var configuration.** Every field in
  `node.yaml` can also be supplied via a `QUPTIME_*` environment
  variable; `qu serve` auto-initialises a fresh data volume from
  these on first start, so `docker compose up` is enough to launch a
  node.
- **Interactive TUI** (`qu tui`) for peers, checks, and alerts with
  live refresh.
- **Hardened systemd unit** shipped via `install.sh`: dedicated
  `quptime` user, `ProtectSystem=strict`, all capabilities dropped by
  default.
- **Multi-arch Docker images** (`linux/amd64`, `linux/arm64`)
  published to `git.cer.sh/axodouble/quptime` (primary) and
  `ghcr.io/axodouble/quptime` (GitHub push-mirror) on every tag.
- **Static Linux binaries** (`amd64`, `arm64`) published per tag with
  a `SHA256SUMS` file to both Gitea Releases (primary) and GitHub
  Releases (mirror). The official installer prefers Gitea, falls back
  to GitHub on failure, and verifies the checksum before placing the
  binary on disk.

### Security

- Cluster secret is compared in constant time
  (`crypto/subtle.ConstantTimeCompare`).
- Self-signed RSA certs minted at `qu init`; SPKI SHA-256
  fingerprints are what's pinned, matching the canonical OpenSSL
  representation.
- Private keys are written with mode `0600`; data and runtime
  directories with `0700`/`0750`.
- All `cluster.yaml` writes go through an atomic `tmpfile + rename`.
- `install.sh` downloads the published `SHA256SUMS` and refuses to
  install if the downloaded binary doesn't match.

### Known limitations

- **Cluster-wide secret distribution.** SMTP passwords and Discord
  webhook URLs configured via `qu alert add …` are stored in
  `cluster.yaml`, which is replicated to every node. Treat every node
  as having read access to every alert credential. Restrict who can
  reach the data directory accordingly. See
  [docs/security.md](docs/security.md) for the threat model.
- **No automatic key rotation.** Rolling a node's identity means
  wiping its data directory, running `qu init` again, and re-adding
  it from another node.
- **No historical metrics.** Only the current aggregate state is kept
  in memory. There is no built-in graph store, SLA calculator, or
  audit log.
- **Master-flap state.** Aggregator hysteresis state lives in
  memory on the current master. When leadership changes the new
  master starts from `StateUnknown` and re-accumulates hysteresis —
  expect a few seconds of delayed alerting after a master switch.
- **No release signing beyond SHA256SUMS** (no cosign / GPG).
  Planned for a future release.

[v0.0.1]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.0.1
