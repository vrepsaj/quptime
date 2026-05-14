#!/bin/bash

# Helper function which echo's all commands before executing them in grayscale prefixed with >
echo_cmd() {
    echo -e "\033[90m> $1\033[0m"
    eval "$1"
}

# Check if jq and curl are installed, if not, error out and ask the user to install them
if ! command -v jq > /dev/null; then
    echo "Error: jq is not installed. Please install jq and try again."
    exit 1
fi
if ! command -v curl > /dev/null; then
    echo "Error: curl is not installed. Please install curl and try again."
    exit 1
fi

# Check if the user is allowed to write to /usr/local/bin, if so, install qu there, else error out and ask the user to install qu manually
if [ -w "/usr/local/bin" ]; then
    # Download the latest release binary from the Git repository and save it to /usr/local/bin/qu
    if command -v curl > /dev/null; then
        release_tag=$(curl -s https://git.cer.sh/api/v1/repos/axodouble/quptime/releases/latest | jq -r '.tag_name')
        echo_cmd "curl -L -o \"/usr/local/bin/qu\" \"https://git.cer.sh/axodouble/quptime/releases/latest/download/qu-$(release_tag)-$(uname -m)\""
        echo_cmd "chmod +x \"/usr/local/bin/qu\""
        echo "> qu has been installed to /usr/local/bin/qu"
        exit 0
    else
        echo "Error: curl is not installed. Please install curl and try again."
        exit 1
    fi
else
    echo "Error: You are not allowed to write to /usr/local/bin. Please install qu manually, or run this script with sudo."
    exit 1
fi

# Check if the user has systemd, if so create a systemd service file for qu serve
if command -v systemctl > /dev/null; then
    echo "> Creating systemd service file for qu serve..."
    cat <<EOL > /etc/systemd/system/qu-serve.service
[Unit]
Description=QUptime Serve
After=network.target

[Service]
ExecStart=/usr/local/bin/qu serve
Restart=always
User=$(whoami)

[Install]
WantedBy=multi-user.target
EOL
    echo_cmd "systemctl daemon-reload"
    echo_cmd "systemctl enable qu-serve.service"
    echo "> qu serve service has been created and enabled. You can start it with 'systemctl start qu-serve.service'"
else
    echo "> Warning: systemd is not available on this system. qu serve will not be automatically started on boot. You can start it manually with '/usr/local/bin/qu serve'"
fi

echo "Installation complete, before starting `qu serve`, make sure to run `qu init` and read the documentation."
