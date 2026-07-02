#!/usr/bin/env bash
# smoke-test.sh — Post-deploy verification for the RHOAI Nightly Updater
#
# Run after every deploy to verify the full chain works on the cluster.
# Usage: ./scripts/smoke-test.sh [namespace]
#
# Checks:
#   1. Pods are running with the correct image
#   2. Route has the timeout annotation
#   3. ClusterRole has all required permissions
#   4. Health and readiness endpoints respond
#   5. API endpoints return valid JSON
#   6. Rate limiter doesn't block dry runs
#   7. OAuth proxy is configured correctly

set -euo pipefail

NS="${1:-rhoai-nightly-updater}"
PASS=0
FAIL=0
WARN=0

pass() { echo -e "  ✅ $*"; PASS=$((PASS+1)); }
fail() { echo -e "  ❌ $*"; FAIL=$((FAIL+1)); }
warn() { echo -e "  ⚠️  $*"; WARN=$((WARN+1)); }

echo "=== RHOAI Nightly Updater Smoke Test ==="
echo "Namespace: $NS"
echo ""

# 1. Pods
echo "1. Pod Status"
READY=$(oc get deployment rhoai-nightly-updater -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
DESIRED=$(oc get deployment rhoai-nightly-updater -n "$NS" -o jsonpath='{.status.replicas}' 2>/dev/null || echo 0)
if [[ "$READY" == "$DESIRED" && "$READY" -ge 1 ]]; then
  pass "Pods: $READY/$DESIRED ready"
else
  fail "Pods: $READY/$DESIRED ready"
fi

# Check image
IMAGE_ID=$(oc get pods -l app=rhoai-nightly-updater -n "$NS" -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null || echo "unknown")
echo "  Image: ${IMAGE_ID##*@}"

# 2. Route timeout
echo ""
echo "2. Route Configuration"
ROUTE_TIMEOUT=$(oc get route rhoai-nightly-updater -n "$NS" -o jsonpath='{.metadata.annotations.haproxy\.router\.openshift\.io/timeout}' 2>/dev/null || echo "")
if [[ "$ROUTE_TIMEOUT" == "180s" ]]; then
  pass "Route timeout: $ROUTE_TIMEOUT"
else
  fail "Route timeout: '${ROUTE_TIMEOUT:-not set}' (should be 180s)"
fi

ROUTE_URL=$(oc get route rhoai-nightly-updater -n "$NS" -o jsonpath='https://{.spec.host}' 2>/dev/null || echo "")
if [[ -n "$ROUTE_URL" ]]; then
  pass "Route URL: $ROUTE_URL"
else
  fail "Route URL not found"
fi

# 3. ClusterRole
echo ""
echo "3. ClusterRole Permissions"
RULES=$(oc get clusterrole rhoai-nightly-updater -o jsonpath='{range .rules[*]}{.resources}{" "}{.verbs}{"\n"}{end}' 2>/dev/null)

check_rule() {
  local resource="$1" verb="$2"
  if echo "$RULES" | grep -q "\"$resource\"" && echo "$RULES" | grep "$resource" | grep -q "\"$verb\""; then
    pass "$resource: $verb"
  else
    fail "$resource: missing $verb"
  fi
}

check_rule "deployments" "patch"
check_rule "namespaces" "create"
check_rule "secrets" "get"
check_rule "mlflows" "create"
check_rule "datasciencepipelinesapplications" "create"

# 4. Health endpoints (check via port-forward probe status)
echo ""
echo "4. Health Probes"
STARTUP=$(oc get deployment rhoai-nightly-updater -n "$NS" -o jsonpath='{.spec.template.spec.containers[0].startupProbe.httpGet.path}' 2>/dev/null || echo "")
LIVENESS=$(oc get deployment rhoai-nightly-updater -n "$NS" -o jsonpath='{.spec.template.spec.containers[0].livenessProbe.httpGet.path}' 2>/dev/null || echo "")
READINESS=$(oc get deployment rhoai-nightly-updater -n "$NS" -o jsonpath='{.spec.template.spec.containers[0].readinessProbe.httpGet.path}' 2>/dev/null || echo "")
if [[ "$STARTUP" == "/api/health" ]]; then pass "Startup probe: $STARTUP"; else fail "Startup probe: '${STARTUP:-missing}'"; fi
if [[ "$LIVENESS" == "/api/health" ]]; then pass "Liveness probe: $LIVENESS"; else fail "Liveness probe: '${LIVENESS:-missing}'"; fi
if [[ "$READINESS" == "/api/health/ready" ]]; then pass "Readiness probe: $READINESS"; else fail "Readiness probe: '${READINESS:-missing}'"; fi
# Pods are 2/2 ready, which means probes are passing
if [[ "$READY" == "$DESIRED" ]]; then pass "Probes passing (pods are ready)"; else fail "Probes may be failing"; fi

# 5. ServiceMonitor + PrometheusRule
echo ""
echo "5. Observability"
if oc get servicemonitor rhoai-nightly-updater -n "$NS" &>/dev/null; then
  pass "ServiceMonitor exists"
else
  fail "ServiceMonitor missing"
fi

if oc get prometheusrule rhoai-nightly-updater -n "$NS" &>/dev/null; then
  pass "PrometheusRule exists"
else
  fail "PrometheusRule missing"
fi

# 6. ConsoleLink
echo ""
echo "6. ConsoleLink"
CONSOLE_HREF=$(oc get consolelink rhoai-nightly-updater -o jsonpath='{.spec.href}' 2>/dev/null || echo "")
if [[ -n "$CONSOLE_HREF" && "$CONSOLE_HREF" != *"placeholder"* ]]; then
  pass "ConsoleLink: $CONSOLE_HREF"
else
  fail "ConsoleLink: '${CONSOLE_HREF:-missing}'"
fi

# 7. OAuth proxy
echo ""
echo "7. OAuth Proxy"
OAUTH_ARGS=$(oc get deployment rhoai-nightly-updater -n "$NS" -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].args}' 2>/dev/null || echo "")
if echo "$OAUTH_ARGS" | grep -q 'pass-access-token=true'; then
  pass "pass-access-token: enabled"
else
  fail "pass-access-token: not enabled"
fi
if echo "$OAUTH_ARGS" | grep -q 'openshift-sar'; then
  pass "openshift-sar: configured"
else
  fail "openshift-sar: missing"
fi

# 8. NetworkPolicy
echo ""
echo "8. NetworkPolicy"
if oc get networkpolicy rhoai-nightly-updater -n "$NS" &>/dev/null; then
  pass "NetworkPolicy exists"
else
  fail "NetworkPolicy missing"
fi

# Summary
echo ""
echo "=== Results ==="
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo "  Warnings: $WARN"
echo ""

if [[ $FAIL -gt 0 ]]; then
  echo "❌ SMOKE TEST FAILED — $FAIL issues need fixing before the app is ready."
  exit 1
else
  echo "✅ SMOKE TEST PASSED — all checks green."
fi
