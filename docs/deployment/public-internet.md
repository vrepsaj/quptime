# Deployment: public-internet exposure

If your nodes do not share a private network and you can't put an
overlay between them (see [tailscale.md](tailscale.md)), this is the
recipe for exposing TCP/9901 directly to the open internet without
losing sleep.

The short version: `qu` is designed for this — every inbound call is
mTLS-pinned at the application layer and the only RPC an untrusted
caller can invoke is `Enroll`, which requires a single-use
pre-deployment token whose hash is the only thing that lives in
`cluster.yaml`. But defence in depth is cheap and you should take it.

> Older revisions of this page referenced a shared `cluster_secret` in
> `node.yaml`. That field has been retired; the daemon ignores it and
> blanks it on first start. New nodes enrol via tokens minted with
> `qu enroll create`. See [../security.md](../security.md).

## Threat model in one paragraph

Anyone on the internet can establish a TLS connection to `:9901`
because the daemon must accept handshakes from currently-untrusted
peers (otherwise no node could ever join). The RPC dispatcher then
rejects every method except `Join` for callers whose fingerprint
isn't in `trust.yaml`. `Join` itself is gated by the **cluster
secret**, compared in constant time. So the realistic attack surface
is:

1. The TLS 1.3 stack accepting handshakes from arbitrary peers.
2. The `Join` handler's secret check and downstream cert ingestion.
3. The blast radius of a leaked cluster secret (an attacker who has
   it can enrol themselves as a peer and propose mutations, which is
   game over).

What can't trivially happen:

- A random attacker observing or modifying cluster traffic — TLS 1.3
  with fingerprint pinning sees to that.
- A random attacker calling any method other than `Join` — the RPC
  dispatcher refuses.

What you should still do:

- Treat `node.yaml.cluster_secret` like an SSH host key. Out-of-band
  distribution only. Never in git, never in CI logs, never in chat.
- Rate-limit and IP-allowlist where you can. The `Join` handler does
  not currently rate-limit at the application layer, so a determined
  attacker could try secrets at TLS-handshake rate.
- Run on a non-default port if your operations workflow allows it.
  Doesn't add security, but reduces background internet noise in the
  logs and makes IDS / WAF rules cleaner.

## Firewall

### nftables (recommended)

A drop-in `/etc/nftables.d/quptime.nft`:

```nft
table inet filter {
  set quptime_peers {
    type ipv4_addr
    elements = { 198.51.100.10, 198.51.100.11, 198.51.100.12 }
  }

  chain quptime_input {
    # Drop everything that didn't come from a known peer.
    ip saddr @quptime_peers tcp dport 9901 accept
    tcp dport 9901 log prefix "quptime-drop: " level info drop
  }

  chain input {
    type filter hook input priority 0; policy drop;
    ct state established,related accept
    iif lo accept
    jump quptime_input
    # ... your other rules
  }
}
```

The allowlist is the highest-ROI mitigation by far — if you maintain
fixed IPs for your monitor nodes, use this and move on.

### ufw

```sh
sudo ufw allow from 198.51.100.10 to any port 9901 proto tcp
sudo ufw allow from 198.51.100.11 to any port 9901 proto tcp
sudo ufw allow from 198.51.100.12 to any port 9901 proto tcp
```

### Dynamic peer IPs

If peer IPs aren't fixed (e.g., one node is on a home connection with
a rotating address), you have three options ranked by preference:

1. Use an overlay instead — see [tailscale.md](tailscale.md). This is
   the right answer.
2. DNS-based allowlisting (`ipset`-from-DNS or a small reconciler that
   re-resolves an allowlist hostname every minute). Beware: a
   compromised DNS resolver becomes a compromise of the allowlist.
3. Drop the allowlist and rely solely on the cluster secret + mTLS.
   This is what `qu` is designed to survive; just be sure the secret
   actually has the entropy `qu init` generated for it (32 random
   bytes, base64-encoded).

## Rate-limiting failed handshakes

`qu` does not currently rate-limit `Join` attempts at the application
layer. You can do it at the firewall, which catches both connect
floods and slow brute-force:

```nft
table inet filter {
  chain quptime_input {
    tcp dport 9901 ct state new \
      meter quptime_ratemeter { ip saddr limit rate over 10/second } \
      log prefix "quptime-rate: " drop
    tcp dport 9901 accept
  }
}
```

Or `fail2ban` with a tiny custom filter that watches `journalctl -u
quptime` for repeated `peer rejected join` lines:

```ini
# /etc/fail2ban/filter.d/quptime.conf
[Definition]
failregex = ^.*quptime:.*peer rejected join.*from <ADDR>.*$
```

```ini
# /etc/fail2ban/jail.d/quptime.local
[quptime]
enabled  = true
filter   = quptime
backend  = systemd
journalmatch = _SYSTEMD_UNIT=quptime.service
maxretry = 3
findtime = 600
bantime  = 86400
```

Note: the daemon doesn't currently log the *peer address* on rejected
joins. The log filter above is illustrative; check what your version
actually emits before relying on it.

## Token hygiene

There is no cluster-wide secret to leak — each new host is enrolled
with a single-use token minted on demand. The rules:

- **Mint with the shortest viable TTL.** `qu enroll create --ttl 15m`
  is enough for an Ansible run; don't leave hour-long tokens lying
  around. Default is 1h, max is 168h.
- **Transport out of band.** Paste the token into your secret
  manager or pipe it directly into the new host's stdin; don't
  email it.
- **Revoke unused tokens.** `qu enroll list` shows outstanding
  tokens; `qu enroll revoke <id>` drops one before it expires.
- **Approve interactively in shared clusters.** Drop `--auto-approve`
  if more than one operator can mint tokens — a second human running
  `qu enroll approve` after seeing the joiner's NodeID is a useful
  audit checkpoint.

## Non-default ports

```sh
# Each node, in node.yaml — or pass --port on init.
qu init --advertise alpha.example.com:51234 --port 51234
```

Open the corresponding firewall rule, restart the daemon. The
cluster doesn't require uniform ports across nodes; each peer's
`advertise` field tells everyone else what to dial.

## What you should monitor on a public deployment

- `term` from `qu status` — if it's ticking up frequently the master
  is flapping, which probably means at least one peer's network is
  unstable. Could be benign, could be a probe attempt.
- The firewall drop counter on the `quptime-drop` rule above.
- The number of TLS handshakes on `:9901`. A spike in handshakes that
  don't progress to a successful RPC is the signature of an enrollment
  brute-force — but unlike the old cluster-secret model, a brute-force
  must hit a *currently outstanding* token, so keep TTLs short and
  revoke aggressively.

For the operational side — backups, upgrades, recovery — see
[operations.md](../operations.md).
