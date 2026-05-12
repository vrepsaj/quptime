#!/bin/bash

# Check if ~/.local/bin exists, if not, create it
if [ ! -d "$HOME/.local/bin" ]; then
    mkdir -p "$HOME/.local/bin"
fi

# Check if ~/.local/bin is in the PATH, if not, give the user a command to add it
if [[ ":$PATH:" != *":$HOME/.local/bin:"* ]]; then
    echo "Please add the following line to your shell configuration file (e.g., ~/.bashrc, ~/.zshrc) to include ~/.local/bin in your PATH:"
    echo 'export PATH="$HOME/.local/bin:$PATH"'
    echo "After adding the line, please restart your terminal or run 'source ~/.bashrc' (or the appropriate command for your shell) to apply the changes."
fi

# Download the binary from git.cer.sh/axodouble/quptime
# Check whether curl or wget is available
if command -v curl > /dev/null; then
    curl -L -o "$HOME/.local/bin/quptime" "https://git.cer.sh/axodouble/quptime/-/raw/main/quptime"
elif command -v wget > /dev/null; then
    wget -O "$HOME/.local/bin/quptime" "https://git.cer.sh/axodouble/quptime/-/raw/main/quptime"
else
    echo "Error: Neither curl nor wget is installed. Please install one of these tools to download the quptime binary."
    exit 1
fi