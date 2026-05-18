# Deployment: Docker / docker-compose

The published image is a 14 MB distroless static container with the
`qu` binary as the entrypoint. It runs as root by default so the
daemon can bind privileged ports and open ICMP sockets; override with
`--user` if your host doesn't need that.

> **Note on cluster joining.** The shared-cluster-secret model
> referenced in some snippets below has been replaced by pre-deployment
> enrollment tokens. Where this page shows `QUPTIME_CLUSTER_SECRET` or
> scraping a secret from logs, the modern flow is:
>
> 1. On any existing cluster node: `qu enroll create --auto-approve` →
>    copy the printed `qu enroll join <token>` command.
> 2. On the new container's host (with the data volume mounted):
>    `docker exec quptime qu enroll join <token> --advertise <addr>`.
>
> The `QUPTIME_CLUSTER_SECRET` env var is silently ignored by recent
> daemons; the daemon also clears any `cluster_secret` field it finds
> in an existing `node.yaml` on first start. See
> [../security.md](../security.md) for the threat model.

## Image references

The same multi-arch (amd64 + arm64) image is published to two
registries. **The Gitea registry is the canonical source** — it also
publishes canary `:master` builds on every branch push. GHCR is a
tag-only push-mirror for users who can't reach `git.cer.sh`.

Primary — Gitea registry:

```
git.cer.sh/axodouble/quptime:master          # tip of main, multi-arch
git.cer.sh/axodouble/quptime:latest          # latest tagged release
git.cer.sh/axodouble/quptime:v0.0.1          # specific tagged release
git.cer.sh/axodouble/quptime:latest-amd64    # single-arch (if you must pin)
```

Fallback — GitHub Container Registry:

```
ghcr.io/axodouble/quptime:latest             # latest tagged release
ghcr.io/axodouble/quptime:v0.0.1             # specific tagged release
ghcr.io/axodouble/quptime:0.0                # latest patch in the 0.0 minor line
```

The image embeds `QUPTIME_DIR=/etc/quptime` and declares it a volume —
treat it as the only piece of state worth persisting.

## Single-node, single-container compose

For a development cluster or a single-node smoke test:

```yaml
# compose.yaml
services:
  quptime:
    image: git.cer.sh/axodouble/quptime:latest
    container_name: quptime
    restart: unless-stopped
    environment:
      # host:port other nodes use to reach this one. Must be reachable
      # from every peer — the loopback inside the container is useless.
      - QUPTIME_ADVERTISE=<host-ip>:9901
      # Pre-shared join secret. Omit on the very first node and read
      # the generated value out of `docker logs quptime`, then set
      # this env var on every follower before bringing them up.
      - QUPTIME_CLUSTER_SECRET=${QUPTIME_CLUSTER_SECRET:-}
    ports:
      - "9901:9901"
    volumes:
      - quptime-data:/etc/quptime
    # ICMP UDP-mode pings need a permissive sysctl on the host:
    #   sysctl net.ipv4.ping_group_range="0 2147483647"
    # Or grant CAP_NET_RAW (more accurate, raw ICMP).
    cap_add:
      - NET_RAW

volumes:
  quptime-data:
```

`qu serve` auto-initialises the data volume on first start using the
`QUPTIME_*` env vars (see [configuration.md](../configuration.md) for
the full list). One command brings everything up:

```sh
docker compose up -d
docker compose exec quptime qu status
```

On the very first node, capture the auto-generated cluster secret:

```sh
docker compose logs quptime | grep -A1 'cluster secret'
```

Copy that value into the `QUPTIME_CLUSTER_SECRET` env var of every
follower before starting them, otherwise their join RPCs will be
rejected. The full list of accepted env vars lives in
[configuration.md](../configuration.md#nodeyaml-field-overrides).

## Three-node compose on a single host

For local testing of the full quorum machinery without three machines:

```yaml
# compose.yaml
x-quptime: &quptime
  image: git.cer.sh/axodouble/quptime:latest
  restart: unless-stopped
  cap_add:
    - NET_RAW

services:
  alpha:
    <<: *quptime
    container_name: alpha
    environment:
      - QUPTIME_ADVERTISE=alpha:9901
      # First node: leave secret unset and read it from `docker logs`.
    ports: ["9901:9901"]
    volumes: ["alpha-data:/etc/quptime"]

  bravo:
    <<: *quptime
    container_name: bravo
    environment:
      - QUPTIME_ADVERTISE=bravo:9901
      - QUPTIME_CLUSTER_SECRET=${SECRET}
    ports: ["9902:9901"]
    volumes: ["bravo-data:/etc/quptime"]

  charlie:
    <<: *quptime
    container_name: charlie
    environment:
      - QUPTIME_ADVERTISE=charlie:9901
      - QUPTIME_CLUSTER_SECRET=${SECRET}
    ports: ["9903:9901"]
    volumes: ["charlie-data:/etc/quptime"]

volumes:
  alpha-data:
  bravo-data:
  charlie-data:
```

Bootstrap:

```sh
# 1. Start alpha first to mint the cluster secret.
docker compose up -d alpha
# 2. Read the secret off alpha's stdout.
export SECRET=$(docker compose logs alpha | awk '/cluster secret/{getline; print $1}')
# 3. Bring up the followers — they pick up the secret from $SECRET.
docker compose up -d bravo charlie

# Invite from alpha. The hostnames resolve over the compose network.
docker compose exec alpha qu node add bravo:9901
sleep 3   # wait for heartbeats before the next add
docker compose exec alpha qu node add charlie:9901

docker compose exec alpha qu status
```

For a cluster on three separate hosts, replicate the compose file on
each box with different `advertise` addresses (the public hostname or
the overlay IP) and bootstrap the same way.

## Multi-host compose

The natural unit is one compose file per host, each running one
`qu` container. The minimum-viable file per host:

```yaml
# /etc/qu-stack/compose.yaml
services:
  quptime:
    image: git.cer.sh/axodouble/quptime:latest
    container_name: quptime
    restart: unless-stopped
    environment:
      - QUPTIME_ADVERTISE=${QUPTIME_ADVERTISE}        # host:9901 reachable from peers
      - QUPTIME_CLUSTER_SECRET=${QUPTIME_CLUSTER_SECRET}
    ports:
      - "9901:9901"
    volumes:
      - /srv/quptime/data:/etc/quptime
    cap_add:
      - NET_RAW
```

Put the per-host values (`QUPTIME_ADVERTISE`, `QUPTIME_CLUSTER_SECRET`)
in a sibling `.env` file or a config-management secret so the compose
file itself is identical across hosts.

Persistence is a bind-mount under `/srv/quptime/data` so backups and
upgrades hit a known path. See [operations.md](../operations.md) for
the backup recipe.

Inter-host traffic on TCP/9901 must be reachable. If the boxes don't
share a private network, prefer the
[Tailscale recipe](tailscale.md) over exposing 9901 directly — see
[public-internet.md](public-internet.md) for the threat model if you
must expose it.

## Behind a reverse proxy

**Don't.** `qu` is mTLS-pinned at the application layer, so a TLS-
terminating proxy would force the daemon to trust whatever cert the
proxy presents — defeating fingerprint pinning. If you need a single
public address per node, use a Layer 4 TCP proxy (`nginx stream`,
HAProxy `mode tcp`, or a plain firewall NAT) that forwards bytes
without touching them.

## Image internals

Build locally if you want to inspect what you're running:

```sh
docker buildx build \
  --build-arg VERSION=$(git describe --tags --always) \
  --platform linux/amd64,linux/arm64 \
  --file docker/Dockerfile \
  --tag quptime:dev \
  --load \
  .
```

The Dockerfile (see `docker/Dockerfile`) is two stages: a `golang:1.24-alpine`
builder that cross-compiles with `-trimpath -ldflags "-s -w"`, and a
`gcr.io/distroless/static-debian12` runtime. No shell, no package
manager, no SSH; you cannot `docker exec -it sh` into it. Use
`docker exec quptime qu ...` for everything.

## Healthcheck

The container exits non-zero if the daemon crashes, so the default
`restart: unless-stopped` policy is enough for liveness. A more
useful readiness check requires the binary to be in your healthchecker:

```yaml
healthcheck:
  test: ["CMD", "/usr/local/bin/qu", "status"]
  interval: 30s
  timeout: 5s
  retries: 3
  start_period: 10s
```

`qu status` exits 0 when the daemon socket is reachable and the
control RPC succeeds — it does **not** fail on quorum loss. That's
intentional: restarting a quorum-less node won't bring quorum back,
and a healthcheck that flaps a follower in and out of `unhealthy`
state every time the master is briefly unreachable is worse than no
check. If you want a stricter readiness signal, pipe `qu status`
through `grep -q 'quorum     true'`.
