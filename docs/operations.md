# Operations

Day-2 tasks: keeping `qu` healthy, upgrading without dropping checks,
backing up state, recovering from failures. Pair this with
[troubleshooting.md](troubleshooting.md) for "the cluster is on fire,
what now" specifics.

## Upgrades

### Rolling upgrade (zero alert loss)

`qu` is built to tolerate one node being absent at a time as long as
quorum still holds. The simple recipe for a 3-node cluster:

```sh
# On each node in turn:
sudo systemctl stop quptime
sudo install -m 0755 qu-new /usr/local/bin/qu
sudo setcap cap_net_raw=+ep /usr/local/bin/qu   # if you use raw ICMP
sudo systemctl start quptime

# Wait for the node to rejoin before moving on:
sudo -u quptime qu status   # should show quorum true, all peers live
```

The first node you upgrade may briefly be a follower with a *higher*
binary version than the master. That's fine as long as no on-disk
format changes; the wire protocol and `cluster.yaml` schema are
stable within a minor version, so minor / patch upgrades freely
interleave.

For major-version upgrades that change the on-disk format, the release
notes will spell out the migration. As of v0 there have been none.

### Downgrades

A node that downgrades to an older binary will refuse to start if
`cluster.yaml` contains fields the older version doesn't know. To
roll back across a schema change, either:

- Take the cluster offline and downgrade all nodes simultaneously.
- Restore a `cluster.yaml` from before the schema change on every node
  before starting the downgraded binary.

Within a single minor version, downgrade is symmetrical with upgrade.

### What can go wrong

- **Restarting two nodes at once in a 3-node cluster** loses quorum.
  No mutations succeed, no alerts fire. Quorum returns the moment
  the second node is back.
- **A node that has been offline for a long time** comes back with a
  stale `cluster.yaml`. It will pull the master's higher version
  within ~1 heartbeat. Don't pre-emptively delete its `cluster.yaml`
  — let the catch-up path handle it.

## Backups

Three files matter, in descending order of "pain if lost":

| File                   | Why back it up                                                       |
| ---------------------- | -------------------------------------------------------------------- |
| `keys/private.pem`     | The node's identity. Lose it and you must re-enroll the host (`qu enroll join`) and rebuild its trust entries cluster-wide. |
| `node.yaml`            | Stable NodeID and advertise. Recoverable, but losing it means the host comes back with a new NodeID. |
| `cluster.yaml`         | Resyncs from any other live peer, so per-node backup is optional.    |

### Per-host backup

```sh
# /etc/cron.daily/quptime-backup
#!/bin/sh
set -eu
dst=/var/backups/quptime/$(date +%Y%m%d)
mkdir -p "$dst"
cp -a /etc/quptime/node.yaml         "$dst/"
cp -a /etc/quptime/keys              "$dst/keys"
cp -a /etc/quptime/cluster.yaml      "$dst/cluster.yaml"
chmod -R go-rwx "$dst"
```

### Cluster-wide backup

The cluster state (`peers`, `checks`, `alerts`) is identical across
every node. Back up one healthy node's `cluster.yaml` and you have
the canonical copy. To restore:

```sh
# Stop the daemon.
sudo systemctl stop quptime

# Drop in the backup. Reset the version to 0 so the running cluster's
# higher version supersedes whatever you're holding — otherwise this
# node will broadcast a stale snapshot and confuse everyone.
sudo cp backup-cluster.yaml /etc/quptime/cluster.yaml
sudo sed -i 's/^version:.*/version: 0/' /etc/quptime/cluster.yaml

sudo systemctl start quptime
# Within seconds the version-observer pulls the live version from a peer.
```

If you're restoring **the entire cluster** (every node lost), the
"reset version to 0" trick doesn't apply — there's no peer with a
higher version. Pick the highest-version backup, restore that file
across every node verbatim, and start the daemons. The cluster will
elect a master and continue.

## Replacing a dead node

A node has died permanently. You want to add a fresh box with the
same role.

1. On a surviving node, evict the dead one:

   ```sh
   sudo -u quptime qu node remove <dead-node-id>
   ```

   This drops it from `cluster.yaml` and removes its trust entry. The
   live set's size shrinks by one — verify quorum still holds.

2. On any surviving node, mint a pre-deployment enrollment token:

   ```sh
   sudo -u quptime qu enroll create --name delta --auto-approve --ttl 1h
   ```

3. On the new host, install `qu` then redeem the token:

   ```sh
   sudo -u quptime qu enroll join <token> --advertise delta.example.com:9901
   sudo systemctl start quptime
   ```

   With `--auto-approve` on the create step, the host is a full peer
   the moment the enrollment RPC succeeds. Without it, run
   `qu enroll approve <id>` on a surviving node first.

The dead node's checks and alerts are unaffected — they live in the
replicated `cluster.yaml`, not the dead node's identity.

## Recovering from lost quorum

You've lost more than half the cluster simultaneously. The remaining
nodes refuse to mutate (correct behaviour: they have no way to know
whether the missing nodes are dead or partitioned).

Options:

- **Bring the missing nodes back.** Always the right first move if it's
  possible. The cluster recovers automatically once enough nodes are
  live.
- **Shrink the cluster.** If you've genuinely lost the missing nodes
  permanently and can't bring them back, you need to manually edit
  `cluster.yaml` on every surviving node to remove the dead peers,
  then restart. Be very deliberate:

  ```sh
  # On each surviving node:
  sudo systemctl stop quptime
  sudoedit /etc/quptime/cluster.yaml   # delete the dead peers[] entries
                                        # bump version to something higher
  sudo systemctl start quptime
  ```

  Make sure every surviving node has identical `cluster.yaml` content
  before restarting any of them. If they don't, you'll get conflicting
  views of who's in the cluster and elections will flap.

- **Start over.** For small clusters this is often faster than the
  manual surgery above: `rm -rf /etc/quptime` everywhere, then
  bootstrap from scratch. You'll lose your checks and alerts unless
  you saved a copy of `cluster.yaml` elsewhere.

## Monitoring `qu` itself

`qu` watches your services. Who watches `qu`?

### From within the cluster

`qu status` is the single source of truth. The fields to watch:

| Field          | Healthy        | Suspicious                                                |
| -------------- | -------------- | --------------------------------------------------------- |
| `quorum`       | `true`         | `false` — no mutations, no alerts.                        |
| `master`       | a NodeID       | `(none — ...)` — quorum lost or election in flight.       |
| `term`         | slow growth    | rapid growth → master flapping, network unstable.         |
| `master` after a restart of the primary | unchanged for ~2 min, then bumps back | bumps back immediately → cooldown disabled or misconfigured. |
| `config ver`   | identical across nodes | divergence → a node is stuck pulling.             |

A simple cron sentinel on each node:

```sh
*/5 * * * * /usr/local/bin/qu status >/dev/null 2>&1 \
  || curl -fsSL -X POST -d "qu down on $(hostname)" https://alert.example.com/oncall
```

### From outside the cluster

`qu` does not currently expose a Prometheus / OpenMetrics endpoint.
The recommended pattern is to run a *separate* tiny monitoring path
that doesn't depend on `qu` — even a single `curl` health check on
each node's :9901 (which is TLS-only; you'll see a handshake succeed
even if the daemon's stuck) catches process death.

To produce structured metrics, write a sidecar that parses `qu status`
output and exports counters. The CLI emits stable, machine-grep-able
output specifically so this is straightforward.

## Operational checklist before you go to bed

After standing up a new cluster, work through:

- [ ] All nodes show `quorum true` in `qu status`.
- [ ] All nodes show identical `config ver`.
- [ ] All nodes show the same `master`.
- [ ] `journalctl -u quptime --since "10 min ago"` has no
      `propose to master:` or `replicate: pull from:` errors.
- [ ] `qu alert test <name>` reaches your inbox / Discord channel for
      every configured alert.
- [ ] At least one check has an intentional failure (a bogus target)
      that you flip back and forth to verify the full state-transition
      → dispatch path end-to-end.
- [ ] Backups of `node.yaml` + `keys/` + `cluster.yaml` are landing in
      your backup destination.
- [ ] Firewall allow-list (if any) lists every peer's IP.
- [ ] You have a documented runbook for adding a new node (mint token
      with `qu enroll create`; redeem with `qu enroll join`) that
      survives the first operator leaving.
