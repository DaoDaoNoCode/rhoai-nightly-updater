#!/usr/bin/env bash
# shellcheck disable=SC2015,SC2016 # pass never fails; a quoted $ is a literal placeholder
# Tests for the Helm chart deploy/chart (docs/GITOPS.md). Offline: helm
# lints and renders the chart locally; nothing talks to a cluster.
#   bash scripts/test-chart.sh
# Needs helm 3.16 or newer. Without helm the copy and version checks still
# run and the rest is skipped, except in CI (CI is set), where it fails.
set -Eeuo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SRC=$(cd "$HERE/.." && pwd)
CHART="$SRC/deploy/chart"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
FAILED=0
PASSED=0
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	FAILED=$((FAILED + 1))
}
pass() { PASSED=$((PASSED + 1)); }
expect_eq() { if [ "$2" = "$3" ]; then pass; else fail "$1: want '$2', got '$3'"; fi; }
finish() {
	printf '%d passed, %d failed\n' "$PASSED" "$FAILED"
	[ "$FAILED" -eq 0 ]
}

# --- Without helm ---------------------------------------------------------------

# The chart reads a copy of the Template (Helm reads files inside the chart
# only); `make chart` writes it.
cmp -s "$SRC/deploy/template.yaml" "$CHART/files/template.yaml" && pass ||
	fail "deploy/chart/files/template.yaml differs from deploy/template.yaml; run 'make chart'"

# The chart's version is the release it belongs to: the top CHANGELOG section
# (dated or not; scripts/release.sh check enforces it at the tag).
chart_field() { sed -n "s/^$1: *\"\{0,1\}\([^\"]*\)\"\{0,1\} *$/\1/p" "$CHART/Chart.yaml" | head -1; }
top=$(sed -n 's/^## \[\([0-9][0-9.]*\)\].*/\1/p' "$SRC/CHANGELOG.md" | head -1)
expect_eq "Chart.yaml version is the top CHANGELOG section" "$top" "$(chart_field version)"
expect_eq "Chart.yaml appVersion" "v$top" "$(chart_field appVersion)"

if ! command -v helm >/dev/null 2>&1; then
	if [ -n "${CI:-}" ]; then
		fail "helm is not installed; CI installs it (HELM_VERSION in .gitlab-ci.yml and .github/workflows/ci.yml)"
	else
		printf 'SKIP: helm is not installed, so the chart was not rendered (https://helm.sh/docs/intro/install/).\n' >&2
	fi
	finish
	exit
fi
helm_version=$(helm version --template '{{.Version}}')
[[ $helm_version =~ ^v3\.(1[6-9]|[2-9][0-9])\. ]] && pass || fail "helm $helm_version: these tests need helm 3.16 or newer (--skip-schema-validation)"

# --- The Template, read by Helm itself ------------------------------------------

# A throwaway chart lists what the Template and the schema hold, with Helm's
# own YAML and JSON parsers, so the expectations below do not depend on the
# chart under test.
INSPECT="$WORK/inspect"
mkdir -p "$INSPECT/templates"
printf 'apiVersion: v2\nname: inspect\nversion: 0.0.0\n' >"$INSPECT/Chart.yaml"
cp "$SRC/deploy/template.yaml" "$INSPECT/template.yaml"
cp "$CHART/values.schema.json" "$INSPECT/schema.json"
cat >"$INSPECT/templates/list.yaml" <<'EOF'
{{- $t := .Files.Get "template.yaml" | fromYaml }}
{{- range $t.objects }}
# object {{ .kind }}/{{ .metadata.name }}/{{ .metadata.namespace | default "" }}
{{- end }}
{{- range $t.parameters }}
# parameter {{ .name }} {{ if .required }}required{{ else }}optional{{ end }} {{ .value | default "" }}
{{- end }}
{{- $s := .Files.Get "schema.json" | fromJson }}
{{- range $k, $_ := $s.properties.parameters.properties }}
# schema {{ $k }}
{{- end }}
EOF
INFO=$(helm template inspect "$INSPECT" 2>&1) || fail "inspect: $INFO"
template_objects=$(sed -n 's/^# object //p' <<<"$INFO")
template_params=$(sed -n 's/^# parameter \([^ ]*\).*/\1/p' <<<"$INFO" | sort)
[ "$(wc -l <<<"$template_objects")" -ge 10 ] && pass || fail "inspect: the Template lists only: $template_objects"
expect_eq "values.schema.json lists exactly the Template's parameters" "$template_params" "$(sed -n 's/^# schema //p' <<<"$INFO" | sort)"
expect_eq "the Template's required parameters" "IMAGE" "$(sed -n 's/^# parameter \([^ ]*\) required.*/\1/p' <<<"$INFO")"
default_of() { sed -n "s/^# parameter $1 [a-z]* //p" <<<"$INFO"; }

# expected_objects NAMESPACE APP_NAME [SESSION CONSOLELINK]: Kind/name/namespace
# of every object a render should have, sorted: the Template's with its
# placeholders replaced, then the hooks that are on (1, the default).
expected_objects() {
	local ns=$1 app=$2 session=${3:-1} link=${4:-1}
	{
		sed -e "s/\${NAMESPACE}/$ns/g" -e "s/\${APP_NAME}/$app/g" \
			-e "s/\${PULL_SECRET_NAMESPACE}/$(default_of PULL_SECRET_NAMESPACE)/g" \
			-e "s/\${MARKETPLACE_NAMESPACE}/$(default_of MARKETPLACE_NAMESPACE)/g" <<<"$template_objects"
		if [ "$session" = 1 ]; then
			printf '%s\n' "ServiceAccount/$app-session-secret/$ns" "Role/$app-session-secret/$ns" \
				"RoleBinding/$app-session-secret/$ns" "Job/$app-session-secret/$ns"
		fi
		if [ "$link" = 1 ]; then
			printf '%s\n' "ServiceAccount/$app-consolelink/$ns" "Role/$app-consolelink/$ns" \
				"RoleBinding/$app-consolelink/$ns" "ClusterRole/$app-$ns-consolelink/" \
				"ClusterRoleBinding/$app-$ns-consolelink/" "Job/$app-consolelink/$ns"
		fi
	} | sort
}

# --- Rendering ------------------------------------------------------------------

DIGEST="sha256:$(printf 'a%.0s' $(seq 64))"
VERSION=v$(chart_field version)
IMAGE="quay.io/example/app:$VERSION@$DIGEST"
PROXY="registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.19@sha256:$(printf 'b%.0s' $(seq 64))"
# The two required values: both images by digest.
BASE=(--set "parameters.IMAGE=$IMAGE" --set "parameters.OAUTH_PROXY_IMAGE=$PROXY")
OUT="$WORK/out.yaml"

# render ARGS...: helm template of the chart into $OUT (stdout and stderr).
render() { helm template rnu "$CHART" "$@" >"$OUT" 2>&1; }

# objects: Kind/name/namespace of every document in $OUT, sorted. The chart
# prints objects with toYaml (sorted keys, two-space indent), so the
# metadata keys are the two-space lines under "metadata:".
objects() {
	awk '
		function flush() { if (kind != "") print kind "/" name "/" ns; kind = name = ns = ""; meta = 0 }
		/^---/ { flush(); next }
		/^kind: / { kind = $2 }
		/^metadata:/ { meta = 1; next }
		/^[^ #]/ { meta = 0 }
		meta && /^  name: / { name = $2 }
		meta && /^  namespace: / { ns = $2 }
		END { flush() }' "$OUT" | sort
}

# expect_render NAME ARGS...: the render succeeds.
expect_render() {
	local name=$1
	shift
	if render "$@"; then pass; else fail "$name: render failed: $(cat "$OUT")"; fi
}
# expect_render_fail NAME TEXT ARGS...: the render fails and prints TEXT.
expect_render_fail() {
	local name=$1 text=$2
	shift 2
	if render "$@"; then
		fail "$name: expected the render to fail"
	elif grep -qF -- "$text" "$OUT"; then
		pass
	else
		fail "$name: expected '$text' in: $(cat "$OUT")"
	fi
}

if helm lint --strict "$CHART" --namespace rhoai-nightly-updater "${BASE[@]}" >"$WORK/lint" 2>&1; then pass; else fail "helm lint: $(cat "$WORK/lint")"; fi

# Only the required values set (the two images): every Template object
# once, with the Template's other defaults, in the release namespace, and
# the two hooks.
expect_render "required values only" --namespace rhoai-nightly-updater "${BASE[@]}"
expect_eq "required values only: the objects" "$(expected_objects rhoai-nightly-updater rhoai-nightly-updater)" "$(objects)"
expect_eq "required values only: each object once" "" "$(objects | uniq -d)"
# (The hook Jobs' shell scripts use ${...} themselves.)
helm template rnu "$CHART" --namespace rhoai-nightly-updater "${BASE[@]}" --show-only templates/objects.yaml >"$WORK/objects.yaml" 2>&1 || true
grep -qF '${' "$WORK/objects.yaml" && fail "required values only: a placeholder is left: $(grep -F '${' "$WORK/objects.yaml")" || pass
grep -qx "        image: $IMAGE" "$OUT" && pass || fail "required values only: the app image is not IMAGE"
grep -qx "        image: $PROXY" "$OUT" && pass || fail "required values only: the oauth-proxy image is not OAUTH_PROXY_IMAGE"
grep -A1 -x '        - name: INSTALL_METHOD' "$OUT" | grep -qx '          value: gitops' && pass || fail "required values only: INSTALL_METHOD is not gitops"
grep -A1 -x '        - name: IMAGE_REPOSITORY' "$OUT" | grep -qx "          value: $(default_of IMAGE_REPOSITORY)" && pass || fail "required values only: IMAGE_REPOSITORY is not the Template's default"
# Every image of the render is pinned by digest, the hooks' included.
expect_eq "every image by digest" "" "$(grep -E '^ *image: ' "$OUT" | grep -vE '@sha256:[0-9a-f]{64}$' || true)"

# NAMESPACE follows the release namespace (the Argo CD destination);
# APP_NAME renames every object.
expect_render "another namespace and app name" --namespace team-a "${BASE[@]}" --set parameters.APP_NAME=upd
expect_eq "another namespace and app name: the objects" "$(expected_objects team-a upd)" "$(objects)"
grep -qF 'up{job="upd", namespace="team-a"}' "$OUT" && pass || fail "the PrometheusRule expression was not substituted"
expect_render "NAMESPACE set explicitly" --namespace team-a "${BASE[@]}" --set parameters.NAMESPACE=team-a
expect_eq "NAMESPACE set explicitly: the objects" "$(expected_objects team-a rhoai-nightly-updater)" "$(objects)"

# Each GitOps extra can be turned off.
expect_render "no session Secret hook" --namespace ns1 "${BASE[@]}" --set gitops.sessionSecret.enabled=false
expect_eq "no session Secret hook: the objects" "$(expected_objects ns1 rhoai-nightly-updater 0 1)" "$(objects)"
expect_render "no ConsoleLink hook" --namespace ns1 "${BASE[@]}" --set gitops.consoleLink.enabled=false
expect_eq "no ConsoleLink hook: the objects" "$(expected_objects ns1 rhoai-nightly-updater 1 0)" "$(objects)"
expect_render "no hooks" --namespace ns1 "${BASE[@]}" --set gitops.sessionSecret.enabled=false --set gitops.consoleLink.enabled=false
expect_eq "no hooks: only the Template's objects" "$(expected_objects ns1 rhoai-nightly-updater 0 0)" "$(objects)"
grep -q 'argocd.argoproj.io/hook' "$OUT" && fail "no hooks: a hook annotation is left" || pass

# A value is placed into its string only: quotes, a backslash, a newline
# and YAML syntax stay part of the value. Helm reads the rendered
# Deployment back and prints the env value as JSON.
printf 'parameters:\n  IMAGE: "%s"\n  OAUTH_PROXY_IMAGE: "%s"\n  RELEASES_URL: "https://example.com/a\\"b\\\\c\\nd: e}"\n' "$IMAGE" "$PROXY" >"$WORK/odd.yaml"
expect_render "a value with quotes and a newline" --namespace ns1 -f "$WORK/odd.yaml"
expect_eq "a value with quotes and a newline: the objects" "$(expected_objects ns1 rhoai-nightly-updater)" "$(objects)"
READBACK="$WORK/readback"
mkdir -p "$READBACK/templates"
printf 'apiVersion: v2\nname: readback\nversion: 0.0.0\n' >"$READBACK/Chart.yaml"
awk '/^---/ { keep = 0 } /^kind: Deployment/ { keep = 1 } keep' "$OUT" >"$READBACK/deployment.yaml"
cat >"$READBACK/templates/env.yaml" <<'EOF'
{{- $d := .Files.Get "deployment.yaml" | fromYaml }}
{{- range (index $d.spec.template.spec.containers 0).env }}
# {{ .name }}={{ .value | toJson }}
{{- end }}
EOF
expect_eq "a value with quotes and a newline: read back" '# RELEASES_URL="https://example.com/a\"b\\c\nd: e}"' \
	"$(helm template readback "$READBACK" | grep '^# RELEASES_URL=')"

# --- What fails -----------------------------------------------------------------

P=(--set "parameters.OAUTH_PROXY_IMAGE=$PROXY")
expect_render_fail "IMAGE is required (schema)" "IMAGE" --namespace ns1 "${P[@]}"
expect_render_fail "IMAGE is required (template)" "parameters.IMAGE is required" --skip-schema-validation --namespace ns1 "${P[@]}"
expect_render_fail "an empty IMAGE (schema)" "IMAGE" --namespace ns1 "${P[@]}" --set-string parameters.IMAGE=
expect_render_fail "an empty IMAGE (template)" "parameters.IMAGE is required" --skip-schema-validation --namespace ns1 "${P[@]}" --set-string parameters.IMAGE=
# OAUTH_PROXY_IMAGE has a tag default in the Template (install.sh replaces
# it with the cluster's version); the chart needs it set, by digest.
expect_render_fail "OAUTH_PROXY_IMAGE is required (schema)" "OAUTH_PROXY_IMAGE" --namespace ns1 --set "parameters.IMAGE=$IMAGE"
expect_render_fail "OAUTH_PROXY_IMAGE is required (template)" "parameters.OAUTH_PROXY_IMAGE is required" \
	--skip-schema-validation --namespace ns1 --set "parameters.IMAGE=$IMAGE"
expect_render_fail "an unknown parameter (schema)" "COOKIE_SECRET" --namespace ns1 "${BASE[@]}" --set parameters.COOKIE_SECRET=x
expect_render_fail "an unknown parameter (template)" "parameters.COOKIE_SECRET is not a parameter of deploy/template.yaml" \
	--skip-schema-validation --namespace ns1 "${BASE[@]}" --set parameters.COOKIE_SECRET=x
expect_render_fail "a misspelled values key" "consoleLinks" --namespace ns1 "${BASE[@]}" --set gitops.consoleLinks.enabled=false
expect_render_fail "a misspelled top-level key" "paramters" --namespace ns1 "${BASE[@]}" --set paramters.APP_NAME=x
expect_render_fail "a non-boolean switch" "enabled" --namespace ns1 "${BASE[@]}" --set-string gitops.consoleLink.enabled=no
# A tag-only image fails to render: the schema refuses it, and so does the
# chart itself when schema validation is skipped.
for skip in "" --skip-schema-validation; do
	expect_render_fail "IMAGE by tag ${skip:-(schema)}" "IMAGE" $skip --namespace ns1 "${P[@]}" --set "parameters.IMAGE=quay.io/example/app:$VERSION"
	expect_render_fail "OAUTH_PROXY_IMAGE by tag ${skip:-(schema)}" "OAUTH_PROXY_IMAGE" $skip --namespace ns1 "${BASE[@]}" \
		--set "parameters.OAUTH_PROXY_IMAGE=$(default_of OAUTH_PROXY_IMAGE)"
	expect_render_fail "gitops.cliImage by tag ${skip:-(schema)}" "cliImage" $skip --namespace ns1 "${BASE[@]}" \
		--set gitops.cliImage=registry.redhat.io/openshift4/ose-cli-rhel9:v4.17
	expect_render_fail "a digest that is not sha256 ${skip:-(schema)}" "IMAGE" $skip --namespace ns1 "${P[@]}" \
		--set "parameters.IMAGE=quay.io/example/app@sha256:abc"
done
expect_render_fail "IMAGE by tag (message)" "parameters.IMAGE must be pinned by digest" --skip-schema-validation --namespace ns1 "${P[@]}" \
	--set "parameters.IMAGE=quay.io/example/app:$VERSION"
expect_render_fail "IMAGE of another release" "holds the template of $VERSION" --namespace ns1 "${P[@]}" --set "parameters.IMAGE=quay.io/example/app:v0.0.1@$DIGEST"
expect_render "IMAGE of another release with allowTemplateMismatch" --namespace ns1 "${P[@]}" --set "parameters.IMAGE=quay.io/example/app:v0.0.1@$DIGEST" --set allowTemplateMismatch=true
expect_render "IMAGE by digest only" --namespace ns1 "${P[@]}" --set "parameters.IMAGE=quay.io/example/app@$DIGEST"
expect_render "IMAGE of a main build" --namespace ns1 "${P[@]}" --set "parameters.IMAGE=quay.io/example/app:main@$DIGEST"
expect_render_fail "allowMutableTag is gone" "allowMutableTag" --namespace ns1 "${BASE[@]}" --set allowMutableTag=true
expect_render_fail "a value with a placeholder" 'must not contain "${"' --namespace ns1 "${BASE[@]}" --set 'parameters.RELEASES_URL=${APP_NAME}'

# A Template the chart cannot substitute fully fails instead of applying a
# literal placeholder.
cp -R "$CHART" "$WORK/chart"
sed 's/value: 127.0.0.1$/value: ${BIND}/' "$SRC/deploy/template.yaml" >"$WORK/chart/files/template.yaml"
if helm template rnu "$WORK/chart" --namespace ns1 "${BASE[@]}" >"$OUT" 2>&1; then
	fail "an undeclared placeholder: the render succeeded"
else
	grep -qF 'placeholders that are not parameters: ${BIND}' "$OUT" && pass || fail "an undeclared placeholder: $(cat "$OUT")"
fi
sed 's/value: 127.0.0.1$/value: ${{APP_NAME}}/' "$SRC/deploy/template.yaml" >"$WORK/chart/files/template.yaml"
if helm template rnu "$WORK/chart" --namespace ns1 "${BASE[@]}" >"$OUT" 2>&1; then
	fail "a \${{NAME}} parameter: the render succeeded"
else
	grep -qF 'non-string' "$OUT" && pass || fail "a \${{NAME}} parameter: $(cat "$OUT")"
fi
rm "$WORK/chart/files/template.yaml"
if helm template rnu "$WORK/chart" --namespace ns1 "${BASE[@]}" >"$OUT" 2>&1; then
	fail "no files/template.yaml: the render succeeded"
else
	grep -qF 'run `make chart`' "$OUT" && pass || fail "no files/template.yaml: $(cat "$OUT")"
fi

# --- The hooks ------------------------------------------------------------------

expect_render "session Secret hook" --namespace ns1 "${BASE[@]}" --show-only templates/session-secret.yaml
grep -c 'argocd.argoproj.io/hook: Sync' "$OUT" | grep -qx 4 && pass || fail "session Secret: not four Sync hooks"
grep -B1 -A1 'argocd.argoproj.io/hook: Sync' "$OUT" | grep -c 'sync-wave: "-2"' | grep -qx 3 && pass || fail "session Secret: the RBAC is not wave -2"
grep -qF 'argocd.argoproj.io/sync-wave: "-1"' "$OUT" && pass || fail "session Secret: the Job is not wave -1"
expect_eq "session Secret: delete policy" 4 "$(grep -c 'hook-delete-policy: BeforeHookCreation,HookSucceeded' "$OUT")"
grep -qF 'resourceNames: ["rhoai-nightly-updater-proxy"]' "$OUT" && pass || fail "session Secret: the Role is not limited to the Secret's name"
grep -qF 'value: "rhoai-nightly-updater-proxy"' "$OUT" && pass || fail "session Secret: the Job does not name the Secret"
grep -qF -- '--from-file=session_secret=' "$OUT" && pass || fail "session Secret: the key is not session_secret"
# The value is never printed: only the length check and the write to the
# memory-backed file use it, and nothing reads the file back.
expect_eq "session Secret: uses of the value" "$(printf '%s\n' 'if [ ${#secret} -ne 32 ]; then' "printf '%s' \"\$secret\" >/secret/session_secret")" \
	"$(grep -F 'secret' "$OUT" | grep -F -e '$secret' -e '${#secret}' | sed 's/^ *//')"
grep -qE 'cat .*/secret/|< */secret/' "$OUT" && fail "session Secret: the file is read back" || pass
grep -qF 'medium: Memory' "$OUT" && pass || fail "session Secret: the value is not kept in memory"
for want in 'runAsNonRoot: true' 'allowPrivilegeEscalation: false' 'drop: ["ALL"]' 'type: RuntimeDefault' 'activeDeadlineSeconds:' 'backoffLimit:' 'limits:'; do
	grep -qF -- "$want" "$OUT" && pass || fail "session Secret Job: no '$want'"
done
grep -q '^ *app: ' "$OUT" && fail "session Secret: the Job pod has the app label the Service selects" || pass

expect_render "ConsoleLink hook" --namespace ns1 "${BASE[@]}" --set parameters.APP_NAME=upd --show-only templates/consolelink.yaml
expect_eq "ConsoleLink: six PostSync hooks" 6 "$(grep -c 'argocd.argoproj.io/hook: PostSync' "$OUT")"
grep -qF 'resourceNames: ["upd-ns1"]' "$OUT" && pass || fail "ConsoleLink: the ClusterRole is not limited to the link's name"
grep -qF 'resourceNames: ["upd"]' "$OUT" && pass || fail "ConsoleLink: the Role is not limited to the Route's name"
grep -qF 'app.kubernetes.io/instance: upd-ns1' "$OUT" && pass || fail "ConsoleLink: the RBAC lacks install.sh's instance label"
grep -qF 'value: "upd-ns1"' "$OUT" && pass || fail "ConsoleLink: the link is not named <APP_NAME>-<NAMESPACE>"
grep -qF 'oc apply --server-side' "$OUT" && pass || fail "ConsoleLink: not server-side apply"
# The link is install.sh's: the same printf lines, from apiVersion to text.
link_lines() { sed -n "/'apiVersion: console.openshift.io\/v1'/,/'  text: /p" "$1" | sed -e 's/^[[:space:]]*//' -e 's/ |.*$//'; }
install_link=$(link_lines "$SRC/scripts/install.sh")
[ "$(wc -l <<<"$install_link")" -ge 10 ] && pass || fail "ConsoleLink: cannot find install.sh's ConsoleLink"
expect_eq "ConsoleLink: the same object as install.sh's" "$install_link" "$(link_lines "$CHART/templates/consolelink.yaml")"

finish
