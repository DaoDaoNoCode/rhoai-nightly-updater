{{- /*
The OpenShift Template this chart renders: files/template.yaml, a copy of
deploy/template.yaml (`make chart` writes it, `make check-chart` and CI fail
when it differs). Helm can only read files inside the chart, hence the copy.
*/ -}}
{{- define "rnu.template" -}}
{{- $t := .Files.Get "files/template.yaml" | fromYaml -}}
{{- if or (not $t) (hasKey $t "Error") (not $t.objects) (not $t.parameters) -}}
{{- fail "files/template.yaml is missing or is not an OpenShift Template; run `make chart`" -}}
{{- end -}}
{{- toJson $t -}}
{{- end -}}

{{- /*
The gitops profile's RBAC and admission policy: files/rbac-gitops.yaml, a
copy of deploy/rbac-gitops.yaml (`make chart`, `make check-chart`), an
OpenShift Template whose parameters are deploy/template.yaml's plus
OPERATOR_NAMESPACE.
*/ -}}
{{- define "rnu.gitopsTemplate" -}}
{{- $t := .Files.Get "files/rbac-gitops.yaml" | fromYaml -}}
{{- if or (not $t) (hasKey $t "Error") (not $t.objects) (not $t.parameters) -}}
{{- fail "files/rbac-gitops.yaml is missing or is not an OpenShift Template; run `make chart`" -}}
{{- end -}}
{{- toJson $t -}}
{{- end -}}

{{- /*
The gitops profile's sign-in, applied to one object of the Template (in
place, before its ${NAME}s are replaced): the sign-in of 3.x, with the app's
ServiceAccount as the OAuth client. The OAuth server allows a ServiceAccount
client only user:info, user:check-access, the list-projects scopes and roles
in its own namespace, so its session token identifies the user and runs the
cluster-admin gate's access review, and can change nothing: the gitops
profile needs no more (its Update acts with the ServiceAccount, whose
admission policy matches that username; its reads are the ServiceAccount's;
pkg/api never asks it to sign in again). So no OAuthClient, no client
Secret and no hook with a right on OAuthClients (templates/oauth-client.yaml
renders only with profile full).
- ServiceAccount ${APP_NAME}: the annotation that makes the Route's host its
  redirect URI (serviceaccounts.openshift.io/oauth-redirectreference).
- Deployment ${APP_NAME}, container oauth-proxy: --client-id and
  --client-secret-file become --openshift-service-account (the client is
  system:serviceaccount:<NAMESPACE>:<APP_NAME>, its secret the pod's token)
  and --scope=user:full becomes --scope=user:info user:check-access;
  the Secret ${APP_NAME}-oauth-client is no longer mounted.
Each replaced argument, the mount and the volume must be there exactly
once, or the render fails: a Template change cannot leave the gitops
profile with half of the full profile's sign-in.
*/ -}}
{{- define "rnu.gitopsSignIn" -}}
{{- if and (eq .kind "ServiceAccount") (eq .metadata.name "${APP_NAME}") -}}
{{- $meta := .metadata -}}
{{- $_ := set $meta "annotations" (merge (dict "serviceaccounts.openshift.io/oauth-redirectreference.primary" "{\"kind\":\"OAuthRedirectReference\",\"apiVersion\":\"v1\",\"reference\":{\"kind\":\"Route\",\"name\":\"${APP_NAME}\"}}") ($meta.annotations | default dict)) -}}
{{- end -}}
{{- if and (eq .kind "Deployment") (eq .metadata.name "${APP_NAME}") -}}
{{- $pod := .spec.template.spec -}}
{{- range $pod.containers -}}
{{- if eq .name "oauth-proxy" -}}
{{- $args := list -}}
{{- $replaced := list -}}
{{- range .args -}}
{{- if eq . "--client-id=${APP_NAME}-${NAMESPACE}" -}}
{{- $args = append $args "--openshift-service-account=${APP_NAME}" -}}
{{- $replaced = append $replaced . -}}
{{- else if eq . "--client-secret-file=/etc/proxy/oauth/client-secret" -}}
{{- $replaced = append $replaced . -}}
{{- else if eq . "--scope=user:full" -}}
{{- $args = append $args "--scope=user:info user:check-access" -}}
{{- $replaced = append $replaced . -}}
{{- else -}}
{{- $args = append $args . -}}
{{- end -}}
{{- end -}}
{{- if or (ne (len $replaced) 3) (ne (len (uniq $replaced)) 3) -}}
{{- fail (printf "profile gitops: the oauth-proxy container of deploy/template.yaml does not have --client-id=${APP_NAME}-${NAMESPACE}, --client-secret-file=/etc/proxy/oauth/client-secret and --scope=user:full once each (found %v); update rnu.gitopsSignIn in deploy/chart/templates/_helpers.tpl" $replaced) -}}
{{- end -}}
{{- $_ := set . "args" $args -}}
{{- $mounts := list -}}
{{- range .volumeMounts -}}
{{- if ne .name "oauth-client" -}}
{{- $mounts = append $mounts . -}}
{{- end -}}
{{- end -}}
{{- if ne (len $mounts) (sub (len .volumeMounts) 1) -}}
{{- fail "profile gitops: the oauth-proxy container of deploy/template.yaml does not mount volume oauth-client once; update rnu.gitopsSignIn in deploy/chart/templates/_helpers.tpl" -}}
{{- end -}}
{{- $_ := set . "volumeMounts" $mounts -}}
{{- end -}}
{{- end -}}
{{- $volumes := list -}}
{{- range $pod.volumes -}}
{{- if not (and (eq .name "oauth-client") (eq (dig "secret" "secretName" "" .) "${APP_NAME}-oauth-client")) -}}
{{- $volumes = append $volumes . -}}
{{- end -}}
{{- end -}}
{{- if ne (len $volumes) (sub (len $pod.volumes) 1) -}}
{{- fail "profile gitops: the Deployment of deploy/template.yaml has no volume oauth-client of Secret ${APP_NAME}-oauth-client; update rnu.gitopsSignIn in deploy/chart/templates/_helpers.tpl" -}}
{{- end -}}
{{- $_ := set $pod "volumes" $volumes -}}
{{- end -}}
{{- end -}}

{{- /*
The Template's parameters, as JSON, the way `oc process` would see them
from install.sh: the Template's defaults, then the chart's own (NAMESPACE is
the release namespace, which Argo CD sets to the destination namespace;
INSTALL_METHOD is gitops), then .Values.parameters. Fails on a key the
Template does not have, a required parameter left empty, an
OAUTH_PROXY_IMAGE left to the Template's tag default, any image not pinned
by digest (IMAGE, OAUTH_PROXY_IMAGE, gitops.cliImage; values.schema.json
checks the same, this holds when schema validation is skipped), and an
IMAGE whose tag names another release than this chart's (unless
allowTemplateMismatch). That last one is a tag-consistency check only: it
catches a tag that names another release, but it does not verify the
digest, and a digest-only or non-release tag (main, a commit) passes it.
A tag vX.Y.Z with a suffix (a pre-release, vX.Y.Z-rc.1) names that exact
version too: a pre-release's chart (appVersion vX.Y.Z-rc.1) refuses the
images of its release and of its other pre-releases, whose templates may
differ, and a release's chart refuses its pre-releases' images.
Nothing at render time can tell which commit a digest was built from
(docs/GITOPS.md says how to check it).
*/ -}}
{{- define "rnu.params" -}}
{{- $t := include "rnu.template" . | fromJson -}}
{{- $params := dict -}}
{{- $required := list -}}
{{- range $t.parameters -}}
{{- $_ := set $params .name (toString (.value | default "")) -}}
{{- if .required -}}
{{- $required = append $required .name -}}
{{- end -}}
{{- end -}}
{{- if hasKey $params "NAMESPACE" -}}
{{- $_ := set $params "NAMESPACE" .Release.Namespace -}}
{{- end -}}
{{- if hasKey $params "INSTALL_METHOD" -}}
{{- $_ := set $params "INSTALL_METHOD" "gitops" -}}
{{- end -}}
{{- if not (has (.Values.profile | default "gitops") (list "gitops" "full")) -}}
{{- fail (printf "profile must be gitops or full, not %v (docs/GITOPS.md, Profiles)" .Values.profile) -}}
{{- end -}}
{{- if hasKey $params "PROFILE" -}}
{{- $_ := set $params "PROFILE" (.Values.profile | default "gitops") -}}
{{- end -}}
{{- if hasKey $params "SUBSCRIPTION_ENV_ALLOWLIST" -}}
{{- $allow := .Values.gitops.subscriptionEnvAllowlist | default list -}}
{{- range $allow -}}
{{- if not (regexMatch "^[A-Za-z_][A-Za-z0-9_]*$" (toString .)) -}}
{{- fail (printf "gitops.subscriptionEnvAllowlist: %q is not an environment variable name ([A-Za-z_][A-Za-z0-9_]*)" (toString .)) -}}
{{- end -}}
{{- end -}}
{{- $_ := set $params "SUBSCRIPTION_ENV_ALLOWLIST" (join "," $allow) -}}
{{- end -}}
{{- range $name, $value := .Values.parameters -}}
{{- if eq $name "PROFILE" -}}
{{- fail "parameters.PROFILE is set by the chart: set the value profile (gitops or full) instead, which also chooses the RBAC" -}}
{{- end -}}
{{- if eq $name "SUBSCRIPTION_ENV_ALLOWLIST" -}}
{{- fail "parameters.SUBSCRIPTION_ENV_ALLOWLIST is set by the chart: set gitops.subscriptionEnvAllowlist instead, which also goes into the admission policy" -}}
{{- end -}}
{{- if not (hasKey $params $name) -}}
{{- fail (printf "parameters.%s is not a parameter of deploy/template.yaml; its parameters are %s" $name (keys $params | sortAlpha | join ", ")) -}}
{{- end -}}
{{- if contains "${" (toString $value) -}}
{{- fail (printf "parameters.%s must not contain \"${\"" $name) -}}
{{- end -}}
{{- $_ := set $params $name (toString $value) -}}
{{- end -}}
{{- range $required -}}
{{- $_ := required (printf "parameters.%s is required (deploy/template.yaml marks it required); see docs/GITOPS.md" .) (index $params .) -}}
{{- end -}}
{{- if not (hasKey (.Values.parameters | default dict) "OAUTH_PROXY_IMAGE") -}}
{{- fail "parameters.OAUTH_PROXY_IMAGE is required: the oauth-proxy image of the cluster's OCP minor version, by digest (registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.<minor>@sha256:<digest>); see docs/GITOPS.md" -}}
{{- end -}}
{{- $images := dict "parameters.IMAGE" $params.IMAGE "parameters.OAUTH_PROXY_IMAGE" $params.OAUTH_PROXY_IMAGE "gitops.cliImage" .Values.gitops.cliImage -}}
{{- range $key, $image := $images -}}
{{- if not (regexMatch "^[^@\\s]+@sha256:[0-9a-f]{64}$" (toString $image)) -}}
{{- fail (printf "%s must be pinned by digest (<repository>@sha256:<digest> or <repository>:<tag>@sha256:<digest>), not %s: Argo CD applies what Git says, and a tag can move between the sync and the pull. See docs/GITOPS.md for finding the digest" $key (toString $image)) -}}
{{- end -}}
{{- end -}}
{{- if hasKey $params "IMAGE" -}}
{{- $image := $params.IMAGE -}}
{{- $tag := regexFind ":v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?@" $image | trimPrefix ":" | trimSuffix "@" -}}
{{- if and $tag (ne $tag .Chart.AppVersion) (not $.Values.allowTemplateMismatch) -}}
{{- fail (printf "the tag of parameters.IMAGE names release %s, but this chart (targetRevision) holds the template of %s: an image only works with the template of its own release. Set the chart's targetRevision to %s, or allowTemplateMismatch: true. (This compares the tag only, not the digest.)" $tag .Chart.AppVersion $tag) -}}
{{- end -}}
{{- end -}}
{{- toJson $params -}}
{{- end -}}

{{- /*
Annotations of an Argo CD hook: "rnu.hook" (dict "phase" "Sync" "wave" "-2").
BeforeHookCreation replaces the previous run's object; HookSucceeded removes
it once the whole operation succeeded (gitops-engine pkg/sync/
sync_context.go: hooks are deleted when the operation ends, not when their
Job ends), so a failed run stays for debugging. "deleteOnFailure" true adds
HookFailed, for RBAC objects whose right should not outlive a failed run:
Argo CD deletes them when the operation fails too, and the failed Job stays
with its log. Without "wave" (PostDelete hooks) there is no sync-wave
annotation: Argo CD creates every PostDelete hook in one pass and orders
nothing (controller/hook.go, executePostDeleteHooks).
*/ -}}
{{- define "rnu.hook" -}}
argocd.argoproj.io/hook: {{ .phase | quote }}
{{- if .wave }}
argocd.argoproj.io/sync-wave: {{ .wave | quote }}
{{- end }}
argocd.argoproj.io/hook-delete-policy: {{ if .deleteOnFailure }}BeforeHookCreation,HookSucceeded,HookFailed{{ else }}BeforeHookCreation,HookSucceeded{{ end }}
{{- end -}}

{{- /*
The pod and container security settings of the hook Jobs. No runAsUser:
OpenShift's restricted-v2 SCC assigns one from the namespace's range.
*/ -}}
{{- define "rnu.hookPodSecurity" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end -}}
{{- define "rnu.hookContainerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}
{{- define "rnu.hookResources" -}}
requests:
  cpu: 10m
  memory: 64Mi
limits:
  cpu: 200m
  memory: 256Mi
{{- end -}}
