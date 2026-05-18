# QUptime documentation

Production-oriented documentation for `qu`, a small distributed uptime
monitor that votes on the health of HTTP/TCP/ICMP targets across a
cluster of cooperating nodes.

The top-level `README.md` is the marketing pitch and quick-start. The
pages here go deeper and are organised by what you're trying to do.

## Getting set up

- [Installation](installation.md) — pre-built binaries, building from
  source, verifying release artifacts, what the install script does.
- [Configuration](configuration.md) — `node.yaml`, `cluster.yaml`,
  `trust.yaml`, environment variables, file layout, defaults.

## Running it

- [Architecture](architecture.md) — how nodes form quorum, how a master
  is elected, how cluster state replicates, what happens during a
  partition, and exactly which guarantees the design gives you.
- [Operations](operations.md) — day-2 tasks: upgrades, backups,
  recovery from a lost node, recovery from a lost quorum, monitoring
  `qu` itself.
- [Security](security.md) — the mTLS trust model, how pre-deployment
  enrollment tokens work, how to rotate keys, what to put on a public
  network and what not to.
- [Troubleshooting](troubleshooting.md) — common failure modes with
  the log lines you'll see and the fix.

## Deployment recipes

Pick the one that matches your environment. They share most of the
operational guidance — what differs is how `qu` is packaged and how
the inter-node link is secured at the network layer.

- [systemd on bare metal / VM](deployment/systemd.md) — single static
  binary, hardened unit file, `CAP_NET_RAW` for ICMP.
- [Docker / docker-compose](deployment/docker.md) — official image,
  single-node and multi-node compose files, persistent volumes.
- [Tailscale / WireGuard overlay](deployment/tailscale.md) — nodes in
  separate networks with no public ingress; cluster traffic stays on
  the tailnet.
- [Public-internet exposure](deployment/public-internet.md) — when
  you have no overlay and `:9901` is reachable from the open
  internet: firewalling, rate-limiting, secret hygiene.

## A note on stability

The wire protocol (`internal/transport`) and the on-disk format
(`cluster.yaml`, `node.yaml`, `trust.yaml`) are considered stable
within a minor version. Breaking changes will bump the major version
and ship with a migration note.
