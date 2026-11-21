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
if ! command -v curl &>/dev/null; then
  echo -e "${RED}ERROR: curl is not installed (dev.sh waits for the backend with it).${NC}"
  exit 1
fi

TOKEN=$(oc whoami -t 2>/dev/null || true)
if [ -z "$TOKEN" ]; then
  echo -e "${RED}ERROR: Not logged into an OpenShift cluster. Run 'oc login' first.${NC}"
  exit 1
fi

SERVER=$(oc whoami --show-server)
echo -e "${GREEN}Cluster:${NC} $SERVER"
# The backend reads the API server's address from KUBERNETES_SERVICE_HOST
# and _PORT, which the kubelet sets in a pod: a bare IP (an IPv6 address
# without brackets; the backend adds them) or name, and a port. Split the
# server URL the same way: https://[fd00::1]:6443 gives fd00::1 and 6443,
# https://api.example.com gives api.example.com and 443.
API_HOSTPORT=${SERVER#https://}
API_HOSTPORT=${API_HOSTPORT%%/*}
case "$API_HOSTPORT" in
  \[*\]:*) API_HOST=${API_HOSTPORT%%]:*}; API_HOST=${API_HOST#[}; API_PORT=${API_HOSTPORT##*]:} ;;
  \[*\])   API_HOST=${API_HOSTPORT#[}; API_HOST=${API_HOST%]}; API_PORT=443 ;;
  *:*)     API_HOST=${API_HOSTPORT%:*}; API_PORT=${API_HOSTPORT##*:} ;;
  *)       API_HOST=$API_HOSTPORT; API_PORT=443 ;;
esac
echo -e "${GREEN}User:${NC}    $(oc whoami)"

# The backend verifies the API server's certificate as oc does. ROSA and OSD
# API certificates are publicly trusted; a self-managed cluster's are signed
# by the cluster's own CA, which only the kubeconfig carries. Pass that CA on
# (KUBE_CA_FILE, see pkg/cluster/client.go); an explicit KUBE_CA_FILE wins.
CA_TMP=""
if [ -z "${KUBE_CA_FILE:-}" ]; then
  kube_cluster_field() { oc config view --raw --minify -o "jsonpath={.clusters[0].cluster.$1}" 2>/dev/null || true; }
  CA_DATA=$(kube_cluster_field certificate-authority-data)
  CA_PATH=$(kube_cluster_field certificate-authority)
  if [ -n "$CA_DATA" ]; then
    CA_TMP=$(mktemp "${TMPDIR:-/tmp}/rhoai-updater-dev-ca.XXXXXX")
    printf '%s' "$CA_DATA" | base64 -d > "$CA_TMP" 2>/dev/null || printf '%s' "$CA_DATA" | base64 -D > "$CA_TMP"
    export KUBE_CA_FILE="$CA_TMP"
  elif [ -n "$CA_PATH" ]; then
    export KUBE_CA_FILE="$CA_PATH"
  elif [ "$(kube_cluster_field insecure-skip-tls-verify)" = true ]; then
    echo -e "${YELLOW}Your oc login skips TLS verification and has no CA, so the backend checks the API certificate"
    echo -e "against the system trust store. That works for ROSA/OSD, but fails for a cluster with its own CA."
    echo -e "Then log in with the cluster's CA (oc login --certificate-authority=FILE), or set KUBE_CA_FILE to it.${NC}"
  fi
fi
echo ""

# One dev server per checkout: a second one would fight over ports 8080,
# 9000 and the S3 port-forward. The lock holds this script's PID.
LOCK_DIR="${TMPDIR:-/tmp}/rhoai-nightly-updater-dev.$(printf '%s' "$PWD" | cksum | cut -d' ' -f1)"
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  OTHER=$(cat "$LOCK_DIR/pid" 2>/dev/null || true)
  if [ -n "$OTHER" ] && kill -0 "$OTHER" 2>/dev/null; then
    echo -e "${RED}ERROR: dev.sh already runs for this checkout (PID $OTHER). Stop it first: kill $OTHER${NC}"
    exit 1
  fi
  # A stale lock from a dev.sh that was killed hard.
  rm -rf "$LOCK_DIR" && mkdir "$LOCK_DIR"
fi
echo $$ > "$LOCK_DIR/pid"

# Install frontend dependencies if needed
if [ ! -d frontend/node_modules ]; then
  echo -e "${YELLOW}Installing frontend dependencies...${NC}"
  (cd frontend && npm ci)
fi

# Cleanup on exit, Ctrl-C, kill or a closed terminal. Each child is stopped
# with its own children: `go run` leaves the compiled server running and
# `npm run dev` leaves webpack running when only the parent is killed.
GO_PID="" NPM_PID="" PF_PID="" WATCHDOG_PID=""
kill_tree() {
  local pid=$1 child
  [ -n "$pid" ] || return 0
  for child in $(pgrep -P "$pid" 2>/dev/null); do kill_tree "$child"; done
  kill "$pid" 2>/dev/null || true
}
CLEANED=""
cleanup() {
  [ -n "$CLEANED" ] && return 0
  CLEANED=1
  echo ""
  echo -e "${YELLOW}Shutting down...${NC}"
  # The watchdog first: it waits for this script, which waits below.
  kill_tree "$WATCHDOG_PID"
  kill_tree "$PF_PID"; kill_tree "$GO_PID"; kill_tree "$NPM_PID"
  wait 2>/dev/null || true
  rm -rf "$LOCK_DIR"
  [ -n "$CA_TMP" ] && rm -f "$CA_TMP"
  echo -e "${GREEN}Done.${NC}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# Keep a port-forward to the S3 storage Service open for the backend's
# bucket request (see the header).
S3_LOCAL_PORT=${S3_LOCAL_PORT:-18333}
# The loop and its port-forward end when this script is gone, even if it
# was killed with -9: the port-forward runs in the background and is
# stopped as soon as this script's PID disappears.
DEV_PID=$$
(
  while kill -0 "$DEV_PID" 2>/dev/null; do
    oc port-forward --address 127.0.0.1 -n seaweedfs svc/seaweedfs-s3 "${S3_LOCAL_PORT}:8333" >/dev/null 2>&1 &
    FWD=$!
    while kill -0 "$DEV_PID" 2>/dev/null && kill -0 "$FWD" 2>/dev/null; do sleep 2; done
    kill "$FWD" 2>/dev/null || true
    wait "$FWD" 2>/dev/null || true
    sleep 1
  done
) &
PF_PID=$!

# Start Go backend in background
echo -e "${CYAN}Starting Go backend on :8080...${NC}"
KUBERNETES_SERVICE_HOST="$API_HOST" \
KUBERNETES_SERVICE_PORT="$API_PORT" \
DEV_TOKEN="$TOKEN" \
DEV_USER="$(oc whoami)" \
DEV_MODE=true \
DEV_S3_ENDPOINT="http://127.0.0.1:${S3_LOCAL_PORT}" \
go run . &
GO_PID=$!

# Start the frontend once the backend answers (any HTTP status: a 503
# "degraded" health is still a running server). go run compiles first, so
# allow up to 3 minutes; stop if the backend exits (a build error).
echo -e "${CYAN}Waiting for the Go backend...${NC}"
for _ in $(seq 1 180); do
  if curl -s -o /dev/null --max-time 2 http://127.0.0.1:8080/api/health; then
    break
  fi
  if ! kill -0 "$GO_PID" 2>/dev/null; then
    echo -e "${RED}ERROR: the Go backend exited; see its output above.${NC}"
    exit 1
  fi
  sleep 1
done
if ! curl -s -o /dev/null --max-time 2 http://127.0.0.1:8080/api/health; then
  echo -e "${RED}ERROR: the Go backend did not answer on 127.0.0.1:8080 within 3 minutes.${NC}"
  exit 1
fi

# Start webpack dev server
echo -e "${CYAN}Starting frontend dev server on :9000...${NC}"
echo ""
echo -e "${GREEN}Open ${YELLOW}http://127.0.0.1:9000${GREEN} in your browser${NC}"
echo -e "(requests are proxied to the Go backend on :8080)"
echo ""
(cd frontend && npm run dev) &
NPM_PID=$!

# A watchdog for `kill -9`, which skips the traps: when this script's PID
# disappears, it stops the backend and the dev server with their children.
(
  while kill -0 "$DEV_PID" 2>/dev/null; do sleep 2; done
  kill_tree "$GO_PID"; kill_tree "$NPM_PID"
) &
WATCHDOG_PID=$!

wait
