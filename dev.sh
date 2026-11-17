#!/usr/bin/env bash
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Local development mode:
# - Go backend on :8080, connects to your current oc cluster
# - Webpack dev server on :9000, proxies /api to :8080
# - Open http://127.0.0.1:9000 in your browser
#
# Since there's no oauth-proxy locally, the Go backend runs with
# DEV_MODE=true and uses your current oc token (DEV_TOKEN) for every
# request. Both servers therefore listen on 127.0.0.1 only.
#
# The backend's one direct S3 request (creating the storage bucket) goes to
# the storage Service, whose cluster DNS name does not resolve here. A
# port-forward to it (127.0.0.1:${S3_LOCAL_PORT:-18333}) is kept open, retried
# until the Service exists and after restarts, and the backend is pointed at
# it with DEV_S3_ENDPOINT (honoured only in dev mode, loopback only).

echo -e "${CYAN}=== RHOAI Nightly Updater — Dev Mode ===${NC}"
echo ""

# Check prerequisites
if ! command -v node &>/dev/null; then
  echo -e "${RED}ERROR: Node.js is not installed. Install it first.${NC}"
  exit 1
fi

TOKEN=$(oc whoami -t 2>/dev/null || true)
if [ -z "$TOKEN" ]; then
  echo -e "${RED}ERROR: Not logged into an OpenShift cluster. Run 'oc login' first.${NC}"
  exit 1
fi

SERVER=$(oc whoami --show-server)
echo -e "${GREEN}Cluster:${NC} $SERVER"
echo -e "${GREEN}User:${NC}    $(oc whoami)"
echo ""

# Install frontend dependencies if needed
if [ ! -d frontend/node_modules ]; then
  echo -e "${YELLOW}Installing frontend dependencies...${NC}"
  (cd frontend && npm install)
fi

# Cleanup on exit
GO_PID="" NPM_PID="" PF_PID=""
cleanup() {
  echo ""
  echo -e "${YELLOW}Shutting down...${NC}"
  kill $GO_PID $NPM_PID 2>/dev/null || true
  # The port-forward loop and its current oc port-forward child.
  pkill -P $PF_PID 2>/dev/null || true
  kill $PF_PID 2>/dev/null || true
  rm -f server
  wait 2>/dev/null || true
  echo -e "${GREEN}Done.${NC}"
}
trap cleanup EXIT

# Keep a port-forward to the S3 storage Service open for the backend's
# bucket request (see the header).
S3_LOCAL_PORT=${S3_LOCAL_PORT:-18333}
(
  while true; do
    oc port-forward --address 127.0.0.1 -n seaweedfs svc/seaweedfs-s3 "${S3_LOCAL_PORT}:8333" >/dev/null 2>&1 || true
    sleep 3
  done
) &
PF_PID=$!

# Start Go backend in background
echo -e "${CYAN}Starting Go backend on :8080...${NC}"
KUBERNETES_SERVICE_HOST=$(echo "$SERVER" | sed 's|https://||' | cut -d: -f1) \
KUBERNETES_SERVICE_PORT=$(echo "$SERVER" | sed 's|https://||' | cut -d: -f2) \
DEV_TOKEN="$TOKEN" \
DEV_USER="$(oc whoami)" \
DEV_MODE=true \
DEV_S3_ENDPOINT="http://127.0.0.1:${S3_LOCAL_PORT}" \
go run . &
GO_PID=$!

# Give the Go backend a moment to start
sleep 1

# Start webpack dev server
echo -e "${CYAN}Starting frontend dev server on :9000...${NC}"
echo ""
echo -e "${GREEN}Open ${YELLOW}http://127.0.0.1:9000${GREEN} in your browser${NC}"
echo -e "(requests are proxied to the Go backend on :8080)"
echo ""
(cd frontend && npm run dev) &
NPM_PID=$!

wait
