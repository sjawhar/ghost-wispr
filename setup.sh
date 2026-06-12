#!/usr/bin/env bash
#
# First-time setup for Ghost Wispr on a Raspberry Pi.
# Run this ON the Pi after cloning the repo. Then run `make deploy`.
#
# Prerequisites:
#   - Tailscale installed and authenticated (tailscale up)
#   - Go, Node.js installed (mise or manual)
#   - PortAudio dev headers (sudo apt install libportaudio2 portaudio19-dev)
#
# Usage:
#   ./setup.sh     # sets up infrastructure
#   make deploy    # builds and deploys the app
#
set -euo pipefail

DEPLOY_DIR="/opt/ghost-wispr"
HOSTNAME=$(tailscale status --self --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['Self']['DNSName'].rstrip('.'))" 2>/dev/null || echo "unknown")
SERVICE_FILE="$HOME/.config/systemd/user/ghost-wispr.service"

echo "=== Ghost Wispr Pi Setup ==="
echo ""

# 1. Create /opt/ghost-wispr
echo "Creating $DEPLOY_DIR..."
sudo mkdir -p "$DEPLOY_DIR"
sudo chown "$(whoami):$(id -gn)" "$DEPLOY_DIR"
mkdir -p "$DEPLOY_DIR/data"

# 2. Copy config files if not already present
if [ ! -f "$DEPLOY_DIR/ghost-wispr.yaml" ]; then
    if [ -f ghost-wispr.yaml.example ]; then
        cp ghost-wispr.yaml.example "$DEPLOY_DIR/ghost-wispr.yaml"
        echo "  Copied ghost-wispr.yaml.example → $DEPLOY_DIR/ghost-wispr.yaml"
        echo "  ⚠  Edit this file to configure your instance."
    else
        echo "  ⚠  No ghost-wispr.yaml.example found. Create $DEPLOY_DIR/ghost-wispr.yaml manually."
    fi
else
    echo "  ghost-wispr.yaml already exists, skipping."
fi

if [ ! -f "$DEPLOY_DIR/.env" ]; then
    if [ -f .env.example ]; then
        cp .env.example "$DEPLOY_DIR/.env"
        echo "  Copied .env.example → $DEPLOY_DIR/.env"
        echo "  ⚠  Edit this file to add your API keys."
    else
        echo "  ⚠  No .env.example found. Create $DEPLOY_DIR/.env manually."
    fi
else
    echo "  .env already exists, skipping."
fi

# 3. Ensure GHOST_WISPR_ADDR is set to localhost-only
if ! grep -q 'GHOST_WISPR_ADDR' "$DEPLOY_DIR/.env" 2>/dev/null; then
    echo "GHOST_WISPR_ADDR=127.0.0.1:8080" >> "$DEPLOY_DIR/.env"
    echo "  Added GHOST_WISPR_ADDR=127.0.0.1:8080 to .env"
fi

# 3b. Install the startup network-readiness gate (used by ExecStartPre below).
echo ""
echo "Installing wait-for-network.sh..."
cp wait-for-network.sh "$DEPLOY_DIR/wait-for-network.sh"
chmod +x "$DEPLOY_DIR/wait-for-network.sh"
echo "  Installed $DEPLOY_DIR/wait-for-network.sh"

# 4. Install systemd user unit
echo ""
echo "Installing systemd user unit..."
mkdir -p "$(dirname "$SERVICE_FILE")"
cat > "$SERVICE_FILE" << EOF
[Unit]
Description=Ghost Wispr continuous transcription
After=network-online.target sound.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=$DEPLOY_DIR
ExecStartPre=$DEPLOY_DIR/wait-for-network.sh
ExecStart=$DEPLOY_DIR/ghost-wispr
EnvironmentFile=$DEPLOY_DIR/.env
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable ghost-wispr
echo "  Installed and enabled ghost-wispr.service"

# 5. Enable lingering (so the service starts on boot without a login session)
echo ""
echo "Enabling lingering for $(whoami)..."
loginctl enable-linger "$(whoami)"

# 6. Set up Tailscale serve (HTTPS proxy, tailnet-only)
echo ""
echo "Setting up Tailscale HTTPS serve..."
tailscale serve --bg --https 443 http://127.0.0.1:8080
echo "  HTTPS available at https://$HOSTNAME/"

echo ""
echo "=== Setup complete ==="
echo ""
echo "  Runtime:    $DEPLOY_DIR/"
echo "  Config:     $DEPLOY_DIR/ghost-wispr.yaml"
echo "  Secrets:    $DEPLOY_DIR/.env"
echo "  Data:       $DEPLOY_DIR/data/"
echo "  HTTPS:      https://$HOSTNAME/"
echo ""
echo "  Next step:  make deploy"
echo ""
echo "  Manage:     systemctl --user start|stop|restart|status ghost-wispr"
echo "  Deploy:     make deploy  (from any workspace)"
echo "  Dev:        make run     (runs on :8081, no interference)"
echo "  Version:    curl -s localhost:8080/api/version"
