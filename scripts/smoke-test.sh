#!/usr/bin/env bash
# smoke-test.sh — Post-deploy verification for the RHOAI Nightly Updater
#
# Run after every deploy or `make upgrade`. Read-only: it only uses
# `oc get` and an in-pod `curl` against the probe listener.
# Usage: ./scripts/smoke-test.sh [namespace] [app-name]
#
# The expected values (route and proxy timeouts, rollout strategy, grace
# period, template revision) are read from deploy/template.yaml, so the
# check fails when the live install drifts from the template.
#
# Checks:
#   1. Deployment matches the template (strategy, grace period, revision)
#   2. Pods are ready; the app reports its build and a current template
#   3. Route and oauth-proxy timeouts, cookie settings and probes
#   4. Least-privilege RBAC objects exist; no legacy ClusterRole remains
#   5. Service and NetworkPolicy expose only 8443 and the metrics port
#   6. Monitoring (warns when user workload monitoring is off)
#   7. ConsoleLink

set -euo pipefail

NS="${1:-rhoai-nightly-updater}"
APP="${2:-rhoai-nightly-updater}"
INSTANCE="${APP}-${NS}"
TEMPLATE="$(cd "$(dirname "$0")/.." && pwd)/deploy/template.yaml"
PASS=0
FAIL=0
WARN=0

pass() { echo -e "  ✅ $*"; PASS=$((PASS+1)); }
fail() { echo -e "  ❌ $*"; FAIL=$((FAIL+1)); }
warn() { echo -e "  ⚠  $*"; WARN=$((WARN+1)); }
expect() { # expect <label> <actual> <wanted>
  if [[ "$2" == "$3" ]]; then pass "$1: $2"; else fail "$1: '${2:-unset}' (template: $3)"; fi
}

[[ -f "$TEMPLATE" ]] || { echo "Template not found: $TEMPLATE"; exit 2; }
# Values from the template
T_ROUTE_TIMEOUT=$(sed -n 's/^ *haproxy.router.openshift.io\/timeout: *//p' "$TEMPLATE" | head -1)
T_UPSTREAM_TIMEOUT=$(sed -n 's/^ *- --upstream-timeout=//p' "$TEMPLATE" | head -1)
T_COOKIE_EXPIRE=$(sed -n 's/^ *- --cookie-expire=//p' "$TEMPLATE" | head -1)
T_STRATEGY=$(sed -n '/^ *strategy:/,/type:/s/^ *type: *//p' "$TEMPLATE" | head -1)
T_GRACE=$(sed -n 's/^ *terminationGracePeriodSeconds: *//p' "$TEMPLATE" | head -1)
T_REVISION=$(sed -n '/name: TEMPLATE_REVISION/{n;s/^ *value: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/p;}' "$TEMPLATE" | head -1)
T_METRICS_PORT=$(sed -n '/name: METRICS_PORT/{n;s/^ *value: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/p;}' "$TEMPLATE" | head -1)

echo "=== RHOAI Nightly Updater Smoke Test ==="
echo "Namespace: $NS  App: $APP"
echo ""

jp() { oc get "$@" 2>/dev/null || true; }
DEPLOY_JSON_ARGS=(deployment "$APP" -n "$NS")

# 1. Deployment vs template
echo "1. Deployment matches deploy/template.yaml"
expect "Strategy" "$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.spec.strategy.type}')" "$T_STRATEGY"
expect "terminationGracePeriodSeconds" "$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.spec.template.spec.terminationGracePeriodSeconds}')" "$T_GRACE"
expect "TEMPLATE_REVISION" "$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="app")].env[?(@.name=="TEMPLATE_REVISION")].value}')" "$T_REVISION"

# 2. Pods and app build
echo ""
echo "2. Pods"
READY=$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.status.readyReplicas}')
DESIRED=$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.status.replicas}')
if [[ -n "$READY" && "$READY" == "$DESIRED" && "$READY" -ge 1 ]]; then
  pass "Pods: $READY/$DESIRED ready"
else
  fail "Pods: ${READY:-0}/${DESIRED:-0} ready"
fi
IMAGE_ID=$(jp pods -l "app=$APP" -n "$NS" -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="app")].imageID}')
echo "  Image: ${IMAGE_ID##*@}"
VERSION_JSON=$(oc exec -n "$NS" "deploy/$APP" -c app -- curl -sf "http://127.0.0.1:${T_METRICS_PORT}/api/version" 2>/dev/null || true)
if [[ -n "$VERSION_JSON" ]]; then
  echo "  Build: $VERSION_JSON"
  if echo "$VERSION_JSON" | grep -q '"templateOutdated":false'; then
    pass "App reports a current template"
  else
    fail "App reports an outdated template; run 'make upgrade'"
  fi
else
  warn "Could not read /api/version from the pod (older image, or no exec permission)"
fi

# 3. Route and oauth-proxy
echo ""
echo "3. Route and OAuth proxy"
expect "Route timeout" "$(jp route "$APP" -n "$NS" -o jsonpath='{.metadata.annotations.haproxy\.router\.openshift\.io/timeout}')" "$T_ROUTE_TIMEOUT"
ROUTE_HOST=$(jp route "$APP" -n "$NS" -o jsonpath='{.spec.host}')
if [[ -n "$ROUTE_HOST" ]]; then pass "Route URL: https://$ROUTE_HOST"; else fail "Route URL not found"; fi
OAUTH_ARGS=$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].args}')
for flag in "--upstream-timeout=$T_UPSTREAM_TIMEOUT" "--cookie-expire=$T_COOKIE_EXPIRE" "--cookie-secret-file=" "--pass-access-token=true" "--openshift-sar="; do
  if [[ "$OAUTH_ARGS" == *"$flag"* ]]; then pass "oauth-proxy $flag"; else fail "oauth-proxy missing $flag"; fi
done
if [[ "$OAUTH_ARGS" == *"--cookie-secret=\$(COOKIE_SECRET)"* ]]; then fail "cookie secret still in the Deployment env"; fi
if jp secret "$APP-proxy" -n "$NS" -o name | grep -q .; then pass "Secret $APP-proxy exists"; else fail "Secret $APP-proxy missing (make upgrade creates it)"; fi
PROXY_PROBE=$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].readinessProbe.httpGet.path}')
expect "oauth-proxy readiness probe" "$PROXY_PROBE" "/oauth/healthz"
APP_PROBE=$(jp "${DEPLOY_JSON_ARGS[@]}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="app")].readinessProbe.httpGet.port}')
expect "App readiness probe port" "$APP_PROBE" "$T_METRICS_PORT"

# 4. RBAC
echo ""
echo "4. RBAC"
if jp clusterrole "$INSTANCE" -o name | grep -q .; then pass "ClusterRole $INSTANCE"; else fail "ClusterRole $INSTANCE missing"; fi
if jp clusterrolebinding "$INSTANCE" -o name | grep -q .; then pass "ClusterRoleBinding $INSTANCE"; else fail "ClusterRoleBinding $INSTANCE missing"; fi
for ns in kube-system openshift-marketplace openshift-ingress; do
  if jp rolebinding "$INSTANCE" -n "$ns" -o name | grep -q .; then pass "RoleBinding in $ns"; else fail "RoleBinding in $ns missing"; fi
done
LEGACY=$(jp clusterrolebinding "$APP" -o jsonpath='{.subjects[0].namespace}')
if [[ "$LEGACY" == "$NS" ]]; then fail "Legacy ClusterRoleBinding $APP still bound to $NS (make upgrade removes it)"; else pass "No legacy ClusterRoleBinding for $NS"; fi
SA="system:serviceaccount:$NS:$APP"
if [[ "$(oc auth can-i list secrets -A --as="$SA" 2>/dev/null || true)" == "no" ]]; then pass "SA cannot list Secrets cluster-wide"; else fail "SA can list Secrets cluster-wide"; fi

# 5. Network exposure
echo ""
echo "5. Network exposure"
SVC_PORTS=$(jp service "$APP" -n "$NS" -o jsonpath='{.spec.ports[*].port}')
expect "Service ports" "$SVC_PORTS" "8443 $T_METRICS_PORT"
NP_PORTS=$(jp networkpolicy "$APP" -n "$NS" -o jsonpath='{.spec.ingress[*].ports[*].port}')
expect "NetworkPolicy ports" "$NP_PORTS" "8443 $T_METRICS_PORT"

# 6. Monitoring
echo ""
echo "6. Monitoring"
if jp servicemonitor "$APP" -n "$NS" -o name | grep -q .; then pass "ServiceMonitor exists"; else fail "ServiceMonitor missing"; fi
if jp prometheusrule "$APP" -n "$NS" -o name | grep -q .; then pass "PrometheusRule exists"; else fail "PrometheusRule missing"; fi
if jp pods -n openshift-user-workload-monitoring -o name | grep -q prometheus; then
  pass "User workload monitoring is running"
else
  warn "User workload monitoring is not enabled: metrics are not scraped and alerts never fire"
fi

# 7. ConsoleLink
echo ""
echo "7. ConsoleLink"
CONSOLE_HREF=$(jp consolelink "$INSTANCE" -o jsonpath='{.spec.href}')
if [[ -n "$CONSOLE_HREF" && "$CONSOLE_HREF" == "https://$ROUTE_HOST" ]]; then
  pass "ConsoleLink: $CONSOLE_HREF"
else
  fail "ConsoleLink $INSTANCE: '${CONSOLE_HREF:-missing}'"
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
