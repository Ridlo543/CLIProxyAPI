#!/usr/bin/env bash
# AinyRouter user installer for Linux (systemd user service).
#   curl -fsSL https://raw.githubusercontent.com/Ridlo543/CLIProxyAPI/<ref>/install-systemd.sh | bash
set -euo pipefail

REPO="Ridlo543/CLIProxyAPI"
INSTALL_BIN="${HOME}/.local/bin"
APP_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/ainyrouter"
SERVICE_DIR="${HOME}/.config/systemd/user"

echo "== AinyRouter installer (Linux) =="

mkdir -p "$INSTALL_BIN" "$APP_DIR/auths" "$APP_DIR/static" "$SERVICE_DIR"

# 1. latest linux tarball
ASSET_URL=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep -o "https://[^\"]*ainyrouter-linux-amd64.tar.gz" | head -n1)
[ -n "${ASSET_URL}" ] || { echo "release asset not found"; exit 1; }
TMP=$(mktemp -d)
curl -fL "$ASSET_URL" -o "$TMP/a.tar.gz"
tar -xzf "$TMP/a.tar.gz" -C "$TMP"
install -m 0755 "$TMP/ainyrouter" "$INSTALL_BIN/ainyrouter"

# 2. config on first run (fresh random management key)
if [ ! -f "$APP_DIR/config.yaml" ]; then
  KEY=$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32)
  cat > "$APP_DIR/config.yaml" <<EOF
host: "127.0.0.1"
port: 18400
auth-dir: "$APP_DIR/auths"
api-keys:
  - "sk-ainy-local"
remote-management:
  secret-key: "$KEY"
  disable-auto-update-panel: true
EOF
  echo
  echo "  your management key: $KEY"
  echo "  (stored in $APP_DIR/config.yaml)"
fi

# 3. panel files from embedded asset
"$INSTALL_BIN/ainyrouter" -panel-install --config "$APP_DIR/config.yaml" >/dev/null

# 4. systemd user unit
cat > "$SERVICE_DIR/ainyrouter.service" <<EOF
[Unit]
Description=AinyRouter proxy server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=${INSTALL_BIN}/ainyrouter --config ${APP_DIR}/config.yaml --no-menu
WorkingDirectory=%h
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now ainyrouter.service

echo
echo "Done. Service status : systemctl --user status ainyrouter"
echo "Dashboard            : http://localhost:18400/"
echo "Logs                 : journalctl --user -u ainyrouter -f"
