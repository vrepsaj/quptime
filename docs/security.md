# Security

The trust model in one page. Read this before deciding where to put
`qu` and who can talk to it.

## What `qu` is trying to defend against

- **Eavesdropping on cluster traffic.** Defended: TLS 1.3 only,
  fingerprint-pinned per peer.
- **MITM on the cluster's inter-node link.** Defended: TLS 1.3 with
  fingerprints pinned during pre-deployment enrollment (the joiner
  receives the cluster's fingerprint inside the enrollment token, not
  out of band).
- **A random internet host enrolling itself as a peer.** Defended:
  enrollment requires a single-use pre-deployment token whose secret
  is stored hashed in cluster.yaml. No shared cluster-wide secret to
  leak.
- **A compromised peer issuing forged cluster-config mutations.** Not
  defended. A peer trusted enough to be in `cluster.yaml.peers` can
  propose mutations through the master. Treat membership as a
  privilege.
- **A compromised peer becoming master.** Election is deterministic on
  the smallest live `NodeID`, so a compromised peer can become master
  if its `NodeID` sorts first. The master can rewrite `cluster.yaml`
  arbitrarily. This is the worst-case blast radius from one compromised
  node.
- **DoS by handshake flood.** Not directly defended at the application
  layer. The TLS stack accepts anyone's handshake; rate-limiting belongs
  at the firewall — see [public-internet.md](deployment/public-internet.md).

## The secrets on disk

| Secret                           | What it is                                | Loss impact                                  |
| -------------------------------- | ----------------------------------------- | -------------------------------------------- |
| `keys/private.pem`               | RSA private key, this node's identity.    | Anyone with it can impersonate this node.    |
| Outstanding enrollment tokens    | Sent by hand to the joiner; valid until TTL or use. | A leaked token can enroll one extra node before it expires; revoke with `qu enroll revoke`. |
| `trust.yaml.entries[].cert_pem`  | Other peers' public certs (not secrets, but they enable mTLS). | Loss only forces re-trust. |

`keys/private.pem` is the only real long-lived secret and lives under
`0600` permissions in the data directory. Back it up; never commit it;
never paste it in chat. Enrollment tokens are short-lived bearer
secrets — treat them like one-time passwords.

Cluster.yaml stores only the **sha256 hash** of each enrollment
secret. An operator who can read cluster.yaml cannot replay a token.

## Why no shared cluster secret

Earlier versions had a single `cluster_secret` in `node.yaml` and any
host with that string could call `Join` and become a peer. That was
one secret per cluster, never rotated automatically, and copied by
hand to every new host — a textbook single point of compromise.

It's gone. New nodes enrol with pre-deployment tokens
([architecture.md](architecture.md) §enrollment). Each token is:

- **Single-use** (consumed on successful submission or operator approval).
- **Time-bound** (default TTL 1h; configurable via `--ttl`).
- **Bound to one cluster** (the token embeds the cluster's leaf-cert
  fingerprints, so a stolen token cannot be redirected to a different
  cluster the attacker controls).
- **Stored hashed** on disk; even reading cluster.yaml does not
  recover the secret.

If you find a `cluster_secret` field still sitting in your
`node.yaml`, the daemon will blank it on next start with a log line —
the field is no longer consulted.

## TLS handshake step by step

For every inter-node call once enrollment has completed:

1. Caller dials peer on its `advertise` address.
2. TLS 1.3 handshake. Both sides present their self-signed leaf cert.
3. The caller's `VerifyPeerCertificate` (set in
   `internal/transport/tls.go`) computes the SPKI fingerprint of the
   server's cert and compares it against `trust.yaml`. If the caller
   knows which `NodeID` it expected, a strict verifier ensures the
   fingerprint matches *that specific* entry — not just any trusted
   peer.
4. The server's TLS layer accepts any client cert (`RequireAnyClientCert`,
   `InsecureSkipVerify: true`) because trust is enforced one layer up.
5. The RPC dispatcher reads the client's cert, computes its
   fingerprint, and looks it up in the server's `trust.yaml`. If no
   entry exists, only the `Enroll` method is permitted.

So:

- An adversary who gets your **public** cert can't impersonate you.
- An adversary who gets your **fingerprint** can't impersonate you.
- An adversary who gets your **private key** *can* impersonate you to
  any peer that trusts your fingerprint.

## Enrollment step by step

Replaces the old TOFU + cluster-secret bootstrap.

1. On any existing cluster node, operator runs:
   ```sh
   qu enroll create --name bravo --ttl 1h
   ```
2. The daemon mints a random 32-byte secret, stores its sha256 hash
   in `cluster.yaml.pending_enrollments`, and prints a token. The
   token is `base64(json)` and contains: the public token ID, the raw
   secret, the cluster's contact endpoints with their pinned TLS
   fingerprints, and the expiry timestamp.
3. The operator copies the token to the new host (out of band — Slack
   DM, an Ansible vault, a wormhole, whatever).
4. On the new host:
   ```sh
   qu enroll join <token> --advertise bravo.example.com:9901
   ```
   This generates local identity (NodeID, RSA keypair, self-signed
   cert), then walks each cluster endpoint until one answers. For
   that endpoint:
   - A TOFU dial fetches the peer's leaf cert.
   - The fingerprint is compared against the one baked into the
     token. If it doesn't match, the endpoint is skipped (could be
     MITM, rotation in flight, or wrong cluster) and the next is
     tried.
   - The matched peer goes into the local trust store so the mTLS
     reconnect is on the trusted code path.
5. The joiner sends an `Enroll` RPC: token id, token secret, and its
   own identity (NodeID, advertise, fingerprint, cert PEM).
6. The cluster (any node — followers forward through the replicator)
   constant-time compares `sha256(presented_secret)` against the
   stored hash, checks expiry, and records the joiner's identity
   under that token.
   - If the token was issued with `--auto-approve`, the cluster
     immediately atomically removes the token and adds the joiner to
     `cluster.yaml.peers`. Replication propagates the new peer to
     every other node, where `daemon.syncTrustFromCluster` populates
     each follower's trust store.
   - Without `--auto-approve`, the cluster records the joiner as a
     pending claim and returns "pending". A cluster operator then
     runs `qu enroll approve <id>` to commit (same atomic mutation).
7. On `Accepted`, the response includes every existing peer's cert
   PEM so the joiner can populate its own trust store before
   `qu serve` even starts. On `Pending`, the joiner already trusts
   the bootstrap peer; the heartbeat loop will pull the full
   `cluster.yaml` once approval lands.

**Trust is acquired from both sides.** The cluster authorises the
joiner by issuing the token (`--auto-approve`) or by `qu enroll
approve` (manual). The joiner authorises the cluster by verifying
the leaf-cert fingerprint against the token before sending anything
secret.

## Revoking and rotating

| Action                                  | How                                                              |
| --------------------------------------- | ---------------------------------------------------------------- |
| Cancel an outstanding token             | `qu enroll revoke <id>` (or just let it expire).                 |
| See what's pending                      | `qu enroll list`                                                 |
| Roll a node's RSA keypair               | Wipe `$QUPTIME_DIR`, generate a fresh token, run `qu enroll join`. The `node_id` will be new; `qu node remove <old-node-id>` evicts the old identity. |

There is no global "cluster secret" to rotate — every enrollment is
its own one-shot credential. If you suspect a single token has
leaked, revoke it and any peer that was added during the leaked
window.

## Identity rotation

To roll a node's RSA keypair (e.g., the private key was on a laptop
that got stolen):

```sh
# On a surviving healthy node, mint a fresh token:
sudo -u quptime qu enroll create --name new-bravo --auto-approve

# On the compromised node, wipe and re-enrol:
sudo systemctl stop quptime
sudo rm -rf /etc/quptime
sudo -u quptime qu enroll join <token> --advertise this-host.example.com:9901
sudo systemctl start quptime

# Back on a healthy node, evict the old identity:
sudo -u quptime qu node remove <old-node-id>
```

The new `node_id` is a fresh UUID; the old one is gone for good. Any
historical references to it (e.g., the `updated_by` field on past
versions of `cluster.yaml`) are cosmetic.

## What the local control socket protects

`$XDG_RUNTIME_DIR/quptime/quptime.sock` (or `/var/run/quptime/...`) is
the channel the CLI uses to talk to the local daemon. It's `0600`
permissioned and authenticated solely by filesystem ACLs — no TLS, no
secrets in the protocol.

Anyone who can `read+write` the socket can:

- Propose cluster mutations (will be relayed to the master).
- Mint enrollment tokens for the cluster.
- Read full cluster state including `cluster.yaml`.
- Trigger test alerts.

So: don't put the daemon's user in a group that other unprivileged
users share. The default systemd setup with a dedicated `quptime`
user gets this right.

## Hardening checklist

- [ ] Dedicated `quptime` system user.
- [ ] Data directory owned by that user, mode 0750.
- [ ] `keys/private.pem` mode 0600.
- [ ] `node.yaml` mode 0600.
- [ ] systemd unit uses `ProtectSystem=strict`, `NoNewPrivileges=true`,
      and the rest of the hardening directives in
      [systemd.md](deployment/systemd.md).
- [ ] If `:9901` is internet-reachable, firewall allow-list to peer
      IPs or use an overlay — see [public-internet.md](deployment/public-internet.md)
      and [tailscale.md](deployment/tailscale.md).
- [ ] Outstanding enrollment tokens carry the shortest TTL that fits
      your deployment pipeline. Revoke any token you no longer need.
- [ ] Backups of `keys/` and `node.yaml` are encrypted at rest.
