#!/usr/bin/env bash
#
# Installs HDHRStream as a systemd service on Linux.
#
#   git clone <repo> && cd HDHRStream && ./install.sh
#
# Re-running is safe: it rebuilds, updates the binary and unit, and leaves your
# existing /etc/hdhrstream.env untouched. Set HDHR_URL=... before running to skip
# the prompt (useful for non-interactive installs).

set -euo pipefail

PREFIX="${PREFIX:-/opt/hdhrstream}"
ENV_FILE="${ENV_FILE:-/etc/hdhrstream.env}"
SERVICE_NAME="hdhrstream"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# Use sudo for system changes unless already root.
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null || die "run as root or install sudo"
  SUDO="sudo"
fi

# --- Prerequisites -----------------------------------------------------------
command -v go     >/dev/null || die "Go is required to build (install 'golang')"
command -v ffmpeg >/dev/null || die "ffmpeg is required at runtime (install 'ffmpeg')"
[ -d /run/systemd/system ]   || die "this installer requires systemd"

# --- Build -------------------------------------------------------------------
info "Building hdhrstream..."
( cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -o hdhrstream . )

# --- Install binary ----------------------------------------------------------
info "Installing binary to ${PREFIX}/hdhrstream"
$SUDO install -d "$PREFIX"
$SUDO install -m 0755 "$SCRIPT_DIR/hdhrstream" "$PREFIX/hdhrstream"

# --- Config ------------------------------------------------------------------
if [ -f "$ENV_FILE" ]; then
  info "Keeping existing config at $ENV_FILE"
else
  url="${HDHR_URL:-}"
  if [ -z "$url" ] && [ -t 0 ]; then
    read -rp "HDHomeRun base URL [http://192.168.1.10]: " url
  fi
  url="${url:-http://192.168.1.10}"

  info "Writing config to $ENV_FILE"
  $SUDO sed "s#^HDHR_URL=.*#HDHR_URL=${url}#" \
    "$SCRIPT_DIR/deploy/hdhrstream.env.example" | $SUDO tee "$ENV_FILE" >/dev/null
  $SUDO chmod 0644 "$ENV_FILE"
  [ "$url" = "http://192.168.1.10" ] && \
    info "Remember to set your real HDHR_URL in $ENV_FILE"
fi

# --- systemd unit ------------------------------------------------------------
info "Installing systemd unit"
$SUDO install -m 0644 "$SCRIPT_DIR/deploy/hdhrstream.service" "$SERVICE_FILE"
$SUDO systemctl daemon-reload
$SUDO systemctl enable "$SERVICE_NAME"
# restart (not just start) so a re-run actually picks up the freshly built binary;
# restart also starts the service if it wasn't already running.
$SUDO systemctl restart "$SERVICE_NAME"

info "Done. The service is running:"
echo
echo "  systemctl status $SERVICE_NAME      # check it"
echo "  journalctl -u $SERVICE_NAME -f      # follow logs"
echo "  sudo nano $ENV_FILE                 # change settings, then:"
echo "  sudo systemctl restart $SERVICE_NAME"
