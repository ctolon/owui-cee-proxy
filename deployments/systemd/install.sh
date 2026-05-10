#!/usr/bin/env bash
# Idempotent installer for the systemd service.
# Usage: sudo ./install.sh /path/to/built/owui-cee-proxy
set -euo pipefail

BIN_SRC="${1:-bin/owui-cee-proxy}"
INSTALL_DIR="/usr/local/bin"
CFG_DIR="/etc/owui-cee-proxy"
UNIT_DIR="/etc/systemd/system"
USER_NAME="owui-cee"
GROUP_NAME="owui-cee"

if [[ $EUID -ne 0 ]]; then
  echo "must run as root" >&2
  exit 1
fi
if [[ ! -f "$BIN_SRC" ]]; then
  echo "binary not found: $BIN_SRC" >&2
  exit 1
fi

# 1. user
if ! id -u "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent \
          --shell /usr/sbin/nologin --user-group "$USER_NAME"
fi

# 2. directories
install -d -m 0755 "$INSTALL_DIR"
install -d -m 0750 -o root -g "$GROUP_NAME" "$CFG_DIR"

# 3. binary
install -m 0755 "$BIN_SRC" "$INSTALL_DIR/owui-cee-proxy"

# 4. config and env
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! -f "$CFG_DIR/config.yaml" ]]; then
  install -m 0640 -o root -g "$GROUP_NAME" \
    "$SCRIPT_DIR/../../configs/config.example.yaml" \
    "$CFG_DIR/config.yaml"
fi
if [[ ! -f "$CFG_DIR/owui-cee-proxy.env" ]]; then
  install -m 0640 -o root -g "$GROUP_NAME" \
    "$SCRIPT_DIR/owui-cee-proxy.env.example" \
    "$CFG_DIR/owui-cee-proxy.env"
  echo "→ edit $CFG_DIR/owui-cee-proxy.env to set secrets" >&2
fi

# 5. unit
install -m 0644 "$SCRIPT_DIR/owui-cee-proxy.service" "$UNIT_DIR/owui-cee-proxy.service"

# 6. activate
systemctl daemon-reload
systemctl enable owui-cee-proxy.service
echo "Installed. Start with: systemctl start owui-cee-proxy" >&2
