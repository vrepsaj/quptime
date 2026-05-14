#!/bin/bash
set -euo pipefail

INSTALL_BIN="/usr/local/bin/qu"
SERVICE_FILE="/etc/systemd/system/qu-serve.service"
SERVICE_USER="${SUDO_USER:-$(whoami)}"
SERVICE_GROUP="$(id -gn "$SERVICE_USER" 2>/dev/null || echo root)"

fail() {
    echo "Error: $*" >&2
    exit 1
}

echo_cmd() {
    echo -e "\033[90m> $1\033[0m"
    eval "$1"
}

require_command() {
    command -v "$1" > /dev/null 2>&1 || fail "$1 is not installed. Please install $1 and try again."
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

require_command jq
require_command curl

if [ ! -w "$(dirname "$INSTALL_BIN")" ]; then
    fail "You are not allowed to write to $(dirname "$INSTALL_BIN"). Run this script with sudo or install qu manually."
fi

RELEASE=$(curl -s https://git.cer.sh/api/v1/repos/axodouble/quptime/releases/latest | jq -r '.tag_name')

echo_cmd "curl -L -o '$INSTALL_BIN' 'https://git.cer.sh/axodouble/quptime/releases/download/${RELEASE}/qu-${RELEASE}-linux-amd64'"
echo_cmd "chmod +x '$INSTALL_BIN'"
echo "> qu has been installed to $INSTALL_BIN"

if "$INSTALL_BIN" --help 2>/dev/null | grep -q "completion"; then
    write_completion bash /usr/share/bash-completion/completions/qu \
    || write_completion bash /etc/bash_completion.d/qu
    write_completion zsh /usr/share/zsh/site-functions/_qu
    write_completion fish /usr/share/fish/vendor_completions.d/qu.fish
else
    echo "> qu does not expose completion support; skipping shell completion installation."
fi

if ! command -v systemctl > /dev/null 2>&1; then
    echo "> Warning: systemd is not available on this system. qu serve will not be automatically started on boot."
    echo "Installation complete, before starting qu serve, make sure to run qu init and read the documentation."
    exit 0
fi

echo "> Creating systemd service file for qu serve..."
cat > "$SERVICE_FILE" <<EOL
[Unit]
Description=QUptime Serve
After=network.target

[Service]
ExecStart=$INSTALL_BIN serve
Restart=always
User=$SERVICE_USER
Group=$SERVICE_GROUP

[Install]
WantedBy=multi-user.target
EOL

echo_cmd "systemctl daemon-reload"
echo_cmd "systemctl enable $(basename "$SERVICE_FILE")"
echo "> qu serve service has been created and enabled. You can start it with 'systemctl start $(basename "$SERVICE_FILE")'"

echo "Installation complete, before starting qu serve, make sure to run qu init and read the documentation."
