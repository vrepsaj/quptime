#!/bin/bash
# QUptime installer.
#
# Downloads the latest released `qu` binary, verifies it against the
# published SHA256SUMS, installs it to /usr/local/bin, and (on systemd
# hosts) drops in a hardened quptime.service that matches the unit
# documented in docs/deployment/systemd.md.
#
# Release sources, tried in order:
#   1. Gitea:    git.cer.sh/axodouble/quptime/releases   (primary — canonical home)
#   2. GitHub:   github.com/Axodouble/QUptime/releases   (push-mirror fallback)
#
# Idempotent — re-running upgrades the binary and refreshes the unit
# without touching the data directory.
set -euo pipefail

INSTALL_BIN="/usr/local/bin/qu"
SERVICE_FILE="/etc/systemd/system/quptime.service"
SERVICE_NAME="$(basename "$SERVICE_FILE")"
SERVICE_USER="quptime"
SERVICE_GROUP="quptime"
DATA_DIR="/etc/quptime"

# Release sources, in preference order. Each row is:
#   <name>|<latest-release API endpoint>|<release-asset base URL>
# The asset URL is concatenated with `/<tag>/<filename>`. Adjust here
# if the project moves hosts.
SOURCES=(
    "gitea|https://git.cer.sh/api/v1/repos/axodouble/quptime/releases/latest|https://git.cer.sh/axodouble/quptime/releases/download"
    "github|https://api.github.com/repos/Axodouble/QUptime/releases/latest|https://github.com/Axodouble/QUptime/releases/download"
)

fail() {
    echo "Error: $*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "$1 is not installed. Please install $1 and try again."
}

write_completion() {
    local shell=$1 path=$2
    [ -d "$(dirname "$path")" ] || return 1
    if "$INSTALL_BIN" completion "$shell" > "$path" 2>/dev/null; then
        echo "> installed $shell completion -> $path"
        return 0
    fi
    rm -f "$path"
    return 1
}

# fetch_from_source tries one release source end-to-end: pulls the
# latest tag from its API, downloads the per-arch binary and the
# accompanying SHA256SUMS, and verifies the checksum. Returns 0 on
# success (with RELEASE and BINARY_NAME set as globals) or 1 if any
# step fails — callers can then try the next source. Stderr is kept
# quiet so a failed primary doesn't spam the operator before the
# fallback is attempted.
fetch_from_source() {
    local api_url=$1
    local release_base=$2
    local tmpdir=$3

    local release
    release=$(curl -fsSL --proto '=https' --tlsv1.2 "$api_url" 2>/dev/null | jq -r '.tag_name' 2>/dev/null) \
        || return 1
    [ -n "$release" ] && [ "$release" != "null" ] || return 1

    local binary_name="qu-${release}-linux-${ARCH}"
    local binary_url="${release_base}/${release}/${binary_name}"
    local sums_url="${release_base}/${release}/SHA256SUMS"

    curl -fsSL --proto '=https' --tlsv1.2 -o "$tmpdir/$binary_name" "$binary_url" 2>/dev/null \
        || return 1
    curl -fsSL --proto '=https' --tlsv1.2 -o "$tmpdir/SHA256SUMS" "$sums_url" 2>/dev/null \
        || return 1

    # Verify against the SHA256SUMS that came from the same source as
    # the binary. Never mix sources here — verifying a GitHub-hosted
    # binary against a Gitea-hosted SHA256SUMS would defeat the
    # tamper check.
    (
        cd "$tmpdir"
        if ! grep -E "[[:space:]]\\*?${binary_name}\$" SHA256SUMS > expected.sum; then
            exit 1
        fi
        if ! sha256sum -c expected.sum >/dev/null 2>&1; then
            exit 1
        fi
    ) || return 1

    RELEASE="$release"
    BINARY_NAME="$binary_name"
    return 0
}

require_command curl
require_command jq
require_command sha256sum
require_command install
require_command mktemp

# --- target architecture ------------------------------------------------
case "$(uname -m)" in
    x86_64)         ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    *)              fail "unsupported architecture: $(uname -m). Pre-built binaries are published for amd64 and arm64 only — build from source for other platforms." ;;
esac

if [ ! -w "$(dirname "$INSTALL_BIN")" ]; then
    fail "Cannot write to $(dirname "$INSTALL_BIN"). Run this script with sudo, or set INSTALL_BIN to a writable location."
fi

# --- download + verify (with fallback) ----------------------------------
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# Globals filled in by fetch_from_source on success.
RELEASE=""
BINARY_NAME=""
INSTALLED_FROM=""
INSTALLED_TMP=""

for source_spec in "${SOURCES[@]}"; do
    IFS='|' read -r src_name src_api src_base <<<"$source_spec"
    src_tmp="$TMPDIR/$src_name"
    mkdir -p "$src_tmp"
    echo "> trying release source: $src_name"
    # `set -e` would abort the whole script the moment fetch_from_source
    # returns nonzero; we want the loop to fall through to the next
    # source instead. Wrap the call so a failure is just data.
    if fetch_from_source "$src_api" "$src_base" "$src_tmp"; then
        INSTALLED_FROM="$src_name"
        INSTALLED_TMP="$src_tmp"
        echo "> $src_name: ${RELEASE} ✓ checksum OK"
        break
    fi
    echo "> $src_name: unavailable"
done

if [ -z "$INSTALLED_FROM" ]; then
    fail "no release source reachable — tried: $(printf '%s ' "${SOURCES[@]%%|*}"). Check network access to git.cer.sh and github.com."
fi

install -m 0755 "$INSTALLED_TMP/$BINARY_NAME" "$INSTALL_BIN"
echo "> qu ${RELEASE} installed to $INSTALL_BIN (source: $INSTALLED_FROM)"

# --- shell completions --------------------------------------------------
if "$INSTALL_BIN" --help 2>/dev/null | grep -q "completion"; then
    write_completion bash /usr/share/bash-completion/completions/qu \
        || write_completion bash /etc/bash_completion.d/qu \
        || true
    write_completion zsh  /usr/share/zsh/site-functions/_qu                 || true
    write_completion fish /usr/share/fish/vendor_completions.d/qu.fish      || true
else
    echo "> qu does not expose completion support; skipping shell completion installation."
fi

# --- systemd unit -------------------------------------------------------
if ! command -v systemctl >/dev/null 2>&1; then
    echo
    echo "> systemd is not available on this system. Installation stops here."
    echo "> Run \`qu serve\` manually (or wire it into the supervisor of your choice)."
    exit 0
fi

# Dedicated service user. Hardened unit drops all capabilities and
# locks the daemon down with ProtectSystem=strict, so it must run as
# its own unprivileged account rather than the invoking sudo user.
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    echo "> creating system user $SERVICE_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0750 "$DATA_DIR"

# Repair ownership and permissions on the data dir's contents. Catches:
#   - re-running the installer over a previous install where the
#     service user/group changed.
#   - the operator ran `qu init` or `qu serve` as root once (easy
#     mistake: `sudo qu init` is shorter than the documented
#     `sudo -u quptime qu init`). When the daemon runs as root its
#     DataDir() resolves to /etc/quptime, so any files it writes land
#     owned by root:root — the systemd service then fails with
#     `open node.yaml: permission denied`.
#   - someone or something (a stray `chmod -R`, a misguided backup
#     restore) tightened or loosened modes. Re-running the installer
#     should be enough to get back to a working baseline.
# The canonical layout (mirrors the modes the daemon writes itself
# in internal/config and internal/crypto):
#   /etc/quptime/                 quptime:quptime  0750
#   /etc/quptime/keys/            quptime:quptime  0700
#   /etc/quptime/node.yaml        quptime:quptime  0600
#   /etc/quptime/cluster.yaml     quptime:quptime  0600
#   /etc/quptime/trust.yaml       quptime:quptime  0600
#   /etc/quptime/keys/private.pem quptime:quptime  0600
#   /etc/quptime/keys/public.pem  quptime:quptime  0644
#   /etc/quptime/keys/cert.pem    quptime:quptime  0644
# The runtime dir /var/run/quptime is owned by systemd via
# RuntimeDirectory= and rebuilt at each service start, so we leave it
# alone.
repair_perms() {
    # Always reset the top-level dir mode — `install -d` only sets it
    # on creation, not on re-run.
    chown "$SERVICE_USER:$SERVICE_GROUP" "$DATA_DIR"
    chmod 0750 "$DATA_DIR"

    # Reassert ownership across the whole tree in one pass.
    if [ -n "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
        chown -R "$SERVICE_USER:$SERVICE_GROUP" "$DATA_DIR"
    fi

    # keys/ is a directory with its own tighter mode.
    if [ -d "$DATA_DIR/keys" ]; then
        chmod 0700 "$DATA_DIR/keys"
    fi

    # Each known file gets its canonical mode if it exists. We don't
    # create anything that isn't already there — that's `qu init`'s
    # job — and we don't touch unknown files an operator may have
    # parked in the dir.
    local f
    for f in node.yaml cluster.yaml trust.yaml keys/private.pem; do
        [ -f "$DATA_DIR/$f" ] && chmod 0600 "$DATA_DIR/$f"
    done
    for f in keys/public.pem keys/cert.pem; do
        [ -f "$DATA_DIR/$f" ] && chmod 0644 "$DATA_DIR/$f"
    done
}

repair_perms
echo "> reasserted ownership ($SERVICE_USER:$SERVICE_GROUP) and modes under $DATA_DIR"

echo "> writing $SERVICE_FILE"
cat > "$SERVICE_FILE" <<'EOF'
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
EOF

systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null
echo "> ${SERVICE_NAME} installed and enabled (not yet started)"

cat <<EOF

Installation complete.

Next steps:

  1. Initialise the node identity. IMPORTANT: run as the ${SERVICE_USER}
     user, not root — otherwise node.yaml lands owned by root and the
     service can't read it on start.

       FIRST node of a brand-new cluster:

         sudo -u ${SERVICE_USER} QUPTIME_DIR=${DATA_DIR} \\
           qu init --advertise <this-host>:9901
         sudo systemctl start ${SERVICE_NAME}

       JOINING an existing cluster — on any existing peer, mint a
       pre-deployment token:

         sudo -u ${SERVICE_USER} qu enroll create --name <this-host> --auto-approve

       …then on THIS host, redeem it:

         sudo -u ${SERVICE_USER} QUPTIME_DIR=${DATA_DIR} \\
           qu enroll join <token> --advertise <this-host>:9901
         sudo systemctl start ${SERVICE_NAME}

     (\`qu enroll join\` does the equivalent of \`qu init\` and submits
     enrollment in one step. See docs/security.md for the threat
     model.)

     If you already ran something as root and the service is failing
     with "permission denied" on node.yaml, repair with:

         sudo chown -R ${SERVICE_USER}:${SERVICE_GROUP} ${DATA_DIR}

  2. Verify it's running:

       sudo -u ${SERVICE_USER} qu status

  3. For ICMP checks, the daemon defaults to unprivileged UDP-mode
     pings — those need the ping_group_range sysctl widened to include
     the ${SERVICE_USER} GID, or grant CAP_NET_RAW in the unit. See
     docs/deployment/systemd.md for the recipes.

Full documentation: https://git.cer.sh/axodouble/quptime/src/branch/master/docs
EOF
