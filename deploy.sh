#!/usr/bin/env bash
set -euo pipefail

PI_HOST="${PI_HOST:-ghost-wispr@ghost-wispr.local}"
REMOTE_DIR="/opt/ghost-wispr"

echo "Building local binary..."
make build

echo "Deploying to $PI_HOST..."
scp ghost-wispr "$PI_HOST:$REMOTE_DIR/"
scp ghost-wispr.service "$PI_HOST:/tmp/"

ssh "$PI_HOST" "
  sudo mv /tmp/ghost-wispr.service /etc/systemd/system/
  sudo systemctl daemon-reload
  sudo systemctl enable ghost-wispr
  sudo systemctl restart ghost-wispr
  sudo systemctl status ghost-wispr --no-pager
"

echo "Done. Web UI at http://$(echo "$PI_HOST" | cut -d@ -f2):8080"
