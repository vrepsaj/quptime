# Deployment: systemd on bare metal / VM

The canonical way to run `qu` on a Linux host. Single static binary,
managed by systemd, with a hardened unit file. Most production users
should start here.

## Audience and assumptions

- You have root (or `sudo`) on the host.
- You have at least three hosts that can reach each other on TCP/9901.
  (Three is the minimum for a useful quorum; fewer is fine for
  development but a 2-node cluster offers no consensus protection.)
- The hosts have a way to authenticate each other — direct IP or a
  resolvable hostname is fine. For overlay networks see
  [tailscale.md](tailscale.md).

## Install the binary

See [installation.md](../installation.md). The official `install.sh`
script writes a *minimal* unit file that's fine for development. For
production replace it with the hardened version below.

## Create a dedicated user

Running as a dedicated unprivileged user is best practice, but ICMP
support adds a wrinkle — see the next section.

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin quptime
sudo install -d -o quptime -g quptime -m 0750 /etc/quptime
sudo install -d -o quptime -g quptime -m 0750 /var/run/quptime
```

## ICMP capabilities

ICMP probes have two implementations:

1. **Unprivileged UDP pings** — Linux's `dgram` ICMP socket. Works on
   any modern kernel without elevated privileges, but only if
   `net.ipv4.ping_group_range` includes the daemon's GID. This is the
   default in `qu`.
2. **Raw ICMP** — requires `CAP_NET_RAW`, more accurate latency
   numbers and works for IPv6 from arbitrary kernels.

The simplest path: stick with unprivileged pings and widen
`ping_group_range`. Sysctl, persistent across reboots:

```sh
# /etc/sysctl.d/10-quptime.conf
net.ipv4.ping_group_range = 0 2147483647
```

```sh
sudo sysctl --system
```

If you need raw ICMP instead, grant the capability on the binary:

```sh
sudo setcap cap_net_raw=+ep /usr/local/bin/qu
```

Note that `setcap` is overwritten by every `qu` upgrade — bake the
`setcap` call into your deploy script, or re-run it after each
package update.

## Hardened unit file

Drop this in `/etc/systemd/system/quptime.service`:

```ini
[Unit]
Description=QUptime distributed uptime monitor
Documentation=https://git.cer.sh/axodouble/quptime
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/qu serve
Restart=always
RestartSec=5s

User=quptime
Group=quptime

# Where state lives. RuntimeDirectory creates /var/run/quptime/ each
# boot owned by User:Group with mode 0750.
Environment=QUPTIME_DIR=/etc/quptime
RuntimeDirectory=quptime
RuntimeDirectoryMode=0750
ReadWritePaths=/etc/quptime /var/run/quptime

# Hardening. Comment out individual directives if a probe needs
# something we've revoked.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

# Network access is required (we're a network monitor). Keep address
# families minimal — AF_NETLINK is needed for some libc lookups.
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK

# If you need raw ICMP, *also* uncomment:
# AmbientCapabilities=CAP_NET_RAW
# CapabilityBoundingSet=CAP_NET_RAW
# Otherwise drop all capabilities:
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
```

Reload systemd and enable:

```sh
sudo systemctl daemon-reload
sudo systemctl enable quptime.service
```

## Initialise the node

**Don't start the service yet** — local identity must exist on disk
first, and it must be created as the `quptime` user so the files
have the right ownership.

On the **first** host of a brand-new cluster:

```sh
sudo -u quptime QUPTIME_DIR=/etc/quptime \
  qu init --advertise alpha.example.com:9901
sudo systemctl start quptime
```

For every **other** host, mint a pre-deployment enrollment token on
`alpha` (or any existing peer) and redeem it on the new host:

```sh
# On alpha:
sudo -u quptime qu enroll create --name bravo --auto-approve --ttl 1h
# → copy the printed `qu enroll join <token>` command.

# On bravo:
sudo -u quptime QUPTIME_DIR=/etc/quptime \
  qu enroll join <paste> --advertise bravo.example.com:9901
sudo systemctl start quptime
```

`qu enroll join` does the equivalent of `qu init` (NodeID +
keypair + cert) and submits the enrollment in one step. With
`--auto-approve` on the create side, the new host is a full peer the
moment the enrollment RPC returns. Drop `--auto-approve` for an
interactive flow that requires `qu enroll approve <id>` on the
cluster side first — see [security.md](../security.md).

## Open the firewall

`qu` needs TCP/9901 reachable between cluster members. Adjust to your
firewall:

```sh
# ufw
sudo ufw allow from <peer-ip> to any port 9901 proto tcp

# firewalld
sudo firewall-cmd --permanent --zone=internal \
  --add-rich-rule='rule family=ipv4 source address=<peer-ip> port port=9901 protocol=tcp accept'
sudo firewall-cmd --reload

# nftables (drop-in)
table inet filter {
  chain input {
    ip saddr { 10.0.0.10, 10.0.0.11, 10.0.0.12 } tcp dport 9901 accept
  }
}
```

For exposing 9901 to the open internet see
[public-internet.md](public-internet.md).

## Start the daemon

```sh
sudo systemctl start quptime
sudo systemctl status quptime
journalctl -u quptime -f
```

## Invite peers

From one node (typically `alpha`):

```sh
sudo -u quptime qu node add bravo.example.com:9901
# Pause a few seconds so heartbeats reach the new peer before the next add —
# otherwise the "needs ≥2 live to mutate" check rejects the second invite.
sudo -u quptime qu node add charlie.example.com:9901
```

`qu node add` prints each remote's fingerprint and asks for SSH-style
confirmation. Verify it matches an out-of-band channel (the remote
operator can show their fingerprint with
`sudo -u quptime qu status` or by reading `trust.yaml`).

## Verify

```sh
sudo -u quptime qu status
```

Expect to see all three peers `live=true` and one of them as
`master`.

## Log scraping

`journalctl -u quptime` is the canonical log stream. Notable lines:

| Pattern                                                       | Meaning                                                   |
| ------------------------------------------------------------- | --------------------------------------------------------- |
| `listening on ... as node ...`                                | Daemon up.                                                |
| `manual-edit: cluster.yaml changed externally — replicating…` | An operator edited `cluster.yaml` directly.               |
| `manual-edit: parse cluster.yaml: ...`                        | Invalid YAML on disk; the operator must fix and re-save.  |
| `report to master ...: <err>`                                 | A follower couldn't ship a probe result to the master.    |
| `replicate: pull from ...: <err>`                             | A follower couldn't pull a higher-version config snapshot. |

## Sample reload / restart drill

After editing the unit file:

```sh
sudo systemctl daemon-reload
sudo systemctl restart quptime
```

After editing `cluster.yaml` by hand:

```sh
sudoedit /etc/quptime/cluster.yaml
# No restart needed — the watcher picks it up within 2s and pushes to master.
```

After upgrading the binary:

```sh
sudo install -m 0755 qu-new /usr/local/bin/qu
sudo setcap cap_net_raw=+ep /usr/local/bin/qu   # if you use raw ICMP
sudo systemctl restart quptime
```

Doing rolling upgrades? See [operations.md](../operations.md).
