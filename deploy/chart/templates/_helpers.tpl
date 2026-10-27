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
{{- range $name, $value := .Values.parameters -}}
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
{{- $tag := regexFind ":v[0-9]+\\.[0-9]+\\.[0-9]+@" $image | trimPrefix ":" | trimSuffix "@" -}}
{{- if and $tag (ne $tag .Chart.AppVersion) (not $.Values.allowTemplateMismatch) -}}
{{- fail (printf "the tag of parameters.IMAGE names release %s, but this chart (targetRevision) holds the template of %s: an image only works with the template of its own release. Set the chart's targetRevision to %s, or allowTemplateMismatch: true. (This compares the tag only, not the digest.)" $tag .Chart.AppVersion $tag) -}}
{{- end -}}
{{- end -}}
{{- toJson $params -}}
{{- end -}}

{{- /*
Annotations of an Argo CD hook: "rnu.hook" (dict "phase" "Sync" "wave" "-2").
BeforeHookCreation replaces the previous run's object; HookSucceeded removes
it once the whole sync succeeded, so a failed run stays for debugging.
*/ -}}
{{- define "rnu.hook" -}}
argocd.argoproj.io/hook: {{ .phase | quote }}
argocd.argoproj.io/sync-wave: {{ .wave | quote }}
argocd.argoproj.io/hook-delete-policy: BeforeHookCreation,HookSucceeded
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
