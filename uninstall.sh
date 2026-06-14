#!/usr/bin/env bash
#
# Removes the HDHRStream systemd service and binary.
# Keeps /etc/hdhrstream.env unless you pass --purge.

set -euo pipefail

PREFIX="${PREFIX:-/opt/hdhrstream}"
ENV_FILE="${ENV_FILE:-/etc/hdhrstream.env}"
SERVICE_NAME="hdhrstream"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

SUDO=""
if [ "$(id -u)" -ne 0 ]; then SUDO="sudo"; fi

$SUDO systemctl disable --now "$SERVICE_NAME" 2>/dev/null || true
$SUDO rm -f "$SERVICE_FILE"
$SUDO systemctl daemon-reload
$SUDO rm -rf "$PREFIX"

if [ "${1:-}" = "--purge" ]; then
  $SUDO rm -f "$ENV_FILE"
  echo "Removed $ENV_FILE"
else
  echo "Left $ENV_FILE in place (use --purge to remove)"
fi

echo "Uninstalled hdhrstream."
