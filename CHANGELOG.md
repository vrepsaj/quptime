# Changelog

All notable changes to this project are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.2.3] — 2026-05-19

### Changed

- **Default alert messages now adapt to the check type.** Instead of
  a single generic format, HTTP, TLS, TCP, ICMP, and DNS each get
  their own built-in subject + body template that surfaces the
  fields that matter for that probe — URL + expected status for
  HTTP, cert state + warn window for TLS, record / resolver /
  expected substring for DNS, etc. Alerts with a custom
  `subject_template` / `body_template` are unaffected.
- The literal source of the per-type templates is included verbatim
  in [`docs/configuration.md`](docs/configuration.md#default-alert-templates-per-check-type)
  so operators can copy one as a starting point for customisation.
  A new exported `alerts.DefaultTemplate(checkType)` returns the raw
  template strings programmatically.

## [v0.2.2] — 2026-05-18

Fourth release of the day, pray this will be the last one for now.

### Changed

- Added a `--beta` flag to the `qu update` command, allowing users to opt-in to receiving pre-release updates. When this flag is set, the update command will check for the absolute latest release, including pre-releases, instead of just the latest stable release.

## [v0.2.1] — 2026-05-18

### Fixed 

- Fixed text not clearing properly in TUI #19

## [v0.2.0] — 2026-05-18

### Added

- **TLS cert-expiry probe** (`qu check add tls`). Dials the target
  over TLS, captures the leaf certificate, and flips DOWN when it is
  expired or within `--warn-days` (default 14) of expiry. Target may
  be a bare host, `host:port`, or a full `https://` URL — bare hosts
  default to `:443`. Chain validity is intentionally not verified, so
  self-signed endpoints work the same way; the check is about
  knowing when the cert needs rotating, not about trust. `--sni`
  overrides the server name when the cert is presented by a different
  hostname than the dial target.
- **DNS probe** (`qu check add dns`). Resolves the target with the
  configured `--record` type (`a` | `aaaa` | `cname` | `mx` | `txt` |
  `ns`, default `a`), optionally via a specific `--resolver`
  (e.g. `1.1.1.1:53`), and flips DOWN on `NXDOMAIN`, an empty answer
  set, or — when `--expect <substring>` is given — when no answer
  contains the required substring (case-insensitive).
- Both new check types are also available via the TUI add/edit
  pickers and via the manual-edit path in `cluster.yaml` (new fields:
  `tls_warn_days`, `tls_server_name`, `dns_record`, `dns_resolver`,
  `dns_expect`; all `omitempty`).
- **`qu check test <id-or-name> [--state down|up|recovered]`** fires
  a synthetic transition for a *real* check through every alert that
  would actually receive it (defaults + explicit `alert_ids` minus
  `suppress_alert_ids`), with a type-aware placeholder `Detail` so
  TLS / DNS / HTTP templates render representative copy. The
  hysteresis filter that normally suppresses Unknown→Up is bypassed
  for tests, so all three verbs (DOWN, RECOVERED, UP) actually fire.
  In the TUI, `t` on the Checks tab opens a picker for the same
  three transitions; `t` on the Alerts tab still sends the simpler
  per-channel synthetic test message it did before. New daemon
  control method `check.test`; new dispatcher method
  `Dispatcher.TestCheck`.
- **Update command** (`qu update`) to update the binary in-place atomically.
- **Pre-deployment enrollment tokens** (`qu enroll create / list /
  approve / revoke / join`). Replace the shared `cluster_secret`
  model with single-use, time-limited, per-token credentials. Trust
  is acquired from both sides: the joiner verifies the cluster's
  TLS fingerprint baked into the token before submitting, and the
  cluster either auto-approves (`--auto-approve`) or requires
  `qu enroll approve <id>` to commit the new peer. Pending
  enrollments live in `cluster.yaml.pending_enrollments`; only the
  sha256 hash of each token's secret is persisted.

### Changed

- **`Enroll` RPC** is the new bootstrap method untrusted peers may
  call. The legacy `Join` RPC is preserved but now returns a
  deprecation error pointing at `qu enroll` — there is no longer
  any cluster-secret path through which a new node can be added.
- **`node.yaml.cluster_secret` and `QUPTIME_CLUSTER_SECRET`** are
  ignored. The daemon blanks any leftover value from `node.yaml`
  on first start after upgrade.

### Removed

- **`qu node add`** (the TOFU + cluster-secret invite). Replaced by
  the `qu enroll` family. The command remains registered as a
  deprecation stub that prints the replacement.
- **`qu init --secret`** flag, and the auto-generated cluster-secret
  print on bootstrap.

### Upgrade notes

Existing clusters keep working with **no operator action** beyond
rolling out the new binary — peer trust lives in `trust.yaml` and
`cluster.yaml.peers[].cert_pem`, neither of which depended on the
cluster secret. On first start of the upgraded daemon you will see
`node.yaml: clearing legacy cluster_secret field (enrollment tokens
replace it)` in the log; that's the entire migration.

Things to know:

- **Don't add new peers — or issue tokens — mid-rollout.** Two
  reasons:
  1. An upgraded node rejects the legacy `Join` RPC, and an
     un-upgraded node hasn't learned `Enroll`, so the actual
     enrollment dance fails at the protocol layer.
  2. The new `pending_enrollments` field in `cluster.yaml` is
     unknown to v0.1.x daemons. If a v0.1.x node holds master
     during the upgrade window and applies any mutation (a new
     check, a peer-edit, a manual cluster.yaml save), it will
     write the file back *without* that field — silently dropping
     any outstanding tokens.

  Finish the rolling upgrade everywhere, then start enrolling new
  hosts. Don't run `qu enroll create` until every node is on
  v0.2.0+.
- **Update your runbooks** for adding nodes:
  - Old: `qu init --advertise … --secret '<paste>'` on new host, then
    `qu node add <new-host>:9901` on an existing node.
  - New: `qu enroll create [--auto-approve]` on an existing node →
    copy the printed `qu enroll join <token>` command and run it on
    the new host.
- **`QUPTIME_CLUSTER_SECRET=…`** lines in compose/systemd env are
  silently ignored. They won't break anything but are misleading;
  drop them when you next touch the file.
- **Automation that called `qu node add` or passed `qu init --secret`**
  will fail loudly — switch it over to `qu enroll`.
- **Scripts that scraped the cluster secret** from `qu init` stderr
  have nothing to scrape now; the new flow mints per-host tokens on
  demand via `qu enroll create`.

`trust.yaml`, `cluster.yaml`, peer relationships, checks, and alerts
are all preserved across the upgrade. The new
`cluster.yaml.pending_enrollments` field is `omitempty` so it does
not appear in clusters that aren't using it.

## [v0.1.2] — 2026-05-18

### Changed

- **TUI upgraded to Bubble Tea v2 / Bubbles v2.** The interactive
  dashboard (`qu tui`) now runs on `charm.land/bubbletea/v2` v2.0.6
  and `charm.land/bubbles/v2` v2.1.0 (with `charm.land/lipgloss/v2`
  v2.0.2). Form inputs now drive a real blinking cursor on first
  paint via the v2 `Focus()` Cmd plumbed through a new `modal.Init()`
  hook, and the alt-screen toggle has moved from a program option
  onto the per-frame `tea.View`. No user-facing keybindings,
  configuration, or daemon protocol changed.

## [v0.1.1] — 2026-05-15

### Changed

- **`install.sh` now repairs data-dir permissions on every run.**
  Re-running the installer reasserts the canonical ownership
  (`quptime:quptime`) and modes across `/etc/quptime/` — `0750` on
  the dir, `0700` on `keys/`, `0600` on `node.yaml`, `cluster.yaml`,
  `trust.yaml`, and `keys/private.pem`, `0644` on `keys/public.pem`
  and `keys/cert.pem`. Makes the installer the one-step recovery
  path when something has tampered with modes (e.g. a stray
  `chmod -R`, a backup restore, or an accidental `sudo qu init`
  that left files owned by root). Unknown files in the dir are left
  alone.

### Fixed

- **CLI socket lookup as the daemon user.** `sudo -u quptime qu …`
  no longer fails with `dial daemon socket /tmp/quptime-quptime/…:
  no such file or directory` while the system daemon is running.
  `config.SocketPath()` now probes the canonical systemd location
  (`/run/quptime/quptime.sock`, then `/var/run/quptime/quptime.sock`)
  regardless of euid before falling back to per-user paths, so the
  CLI reaches the daemon's socket even when `sudo` has stripped
  `RUNTIME_DIRECTORY` and `XDG_RUNTIME_DIR` from the environment.

## [v0.1.0] — 2026-05-15

### Changed

- **Master election cooldown (2 min).** A returning peer with a
  lower NodeID no longer reclaims master the instant it reappears.
  It must stay continuously live for `DefaultMasterCooldown`
  (2 minutes) before displacing the incumbent. Bootstrap and
  quorum-regained-from-empty still elect immediately; the cooldown
  only protects an active incumbent. Fixes #3: a self-monitoring
  master (TCP check on its own `:9901`) would otherwise flap the
  role in lock-step with its own restart.

### Fixed

- #1 Previously up services are alerted as going back up if the master goes down.
  Ignore `unknown` -> `up` transitions during master election; still
  alert on `unknown` -> `down` by design.

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
[v0.1.0]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.1.0
[v0.1.1]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.1.1
[v0.1.2]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.1.2
[v0.2.0]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.2.0
[v0.2.1]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.2.1
[v0.2.2]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.2.2
[v0.2.3]: https://git.cer.sh/axodouble/quptime/releases/tag/v0.2.3