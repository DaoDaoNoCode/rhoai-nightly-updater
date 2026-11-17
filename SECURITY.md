# Security Model

The updater runs as one pod with two containers: **oauth-proxy** (OpenShift sign-in) and **app** (the Go backend and the static UI). Users act through their own identity. Cluster changes are made with the app's ServiceAccount (SA) token, and only after the user's own token has proven that the user is a cluster-admin.

## Who can do what

| Who | Can |
|---|---|
| Anyone who signs in and can `list pods` in `redhat-ods-operator` (the oauth-proxy `--openshift-sar` gate) | Open the UI and read everything it shows (read-only) |
| Users with the **cluster-admin** role | Also run every action that changes the cluster |

Changes require cluster-admin because the tool does cluster-admin work on the user's behalf. It installs an operator (OLM grants whatever the CSV asks for), writes the node pull secret in `kube-system`, deletes admission webhooks and patches CRDs. Namespace `admin`/`edit` and `dedicated-admin` are not enough.

## Authentication and authorization flow

1. The browser opens the Route. oauth-proxy runs the OpenShift OAuth flow and checks the SAR gate above.
2. oauth-proxy forwards the request to the app on `127.0.0.1:8080` with `X-Forwarded-Access-Token` (the user's OAuth token from its own session) and `X-Forwarded-User`.
3. The app resolves the user from the token (`GET user.openshift.io/v1/users/~`, cached for 1 minute per token). `X-Forwarded-User` is not trusted for identity. Every API request is authenticated this way, `/api/pageview` included.
4. Every cluster-changing request (every non-GET route except `POST /api/pageview`, including the Update dry run) also runs a SubjectAccessReview **with the user's token** (`authorization.openshift.io` SAR, `scopes: []`) for verb `*`, resource `*`, group `*`, cluster-wide. That is the cluster-admin role's own rule. A denied user gets 403 "Read-only access". If the review cannot run, the user gets 503 and nothing changes. `TestEveryNonGetRouteIsGated` checks that every route is gated.
5. Only then does the app act, with the SA token.

Token rules (pkg/api/identity.go, handlers.go):

- In the cluster, a request that carries `Authorization: Bearer` is refused with 401. oauth-proxy never forwards a client's bearer token, so such a request did not come through it. The `Authorization: Basic` header that oauth-proxy itself sets (`--pass-basic-auth`, default true) is ignored, not refused.
- Bearer tokens are accepted only with `DEV_MODE=true` (local development, no proxy).
- The template must not add `--openshift-delegate-urls` or `--pass-user-bearer-token` to oauth-proxy, because both accept client tokens. `TestOAuthProxyForwardsOnlyItsOwnSessionToken` pins this.

## ServiceAccount RBAC

`deploy/template.yaml` holds exactly the requests the code makes. `pkg/cluster/rbac_coverage_test.go` derives every API call from the code and fails when a rule is missing or unused. Cluster-scoped names carry the namespace (`<APP_NAME>-<NAMESPACE>`), so two installs do not collide.

**Namespaced Roles:**

| Namespace | Rules |
|---|---|
| the app's namespace | ConfigMaps: `create`; `get` on `rhoai-nightly-updater-{activity,snapshot,operation}`, `update` activity, `patch` snapshot and operation |
| `kube-system` | Secret `additional-pull-secret`: `get`, `patch` |
| `openshift-marketplace` | CatalogSources: `get list create patch delete`; PackageManifests: `list` |
| `openshift-ingress` (`INGRESS_NAMESPACE`) | Gateways: `create`; `get patch delete` on `openshift-ai-inference`. ConfigMaps: `create`; `get patch delete` on `openshift-ai-inference-options`. DNSRecords: `list`. AuthPolicy `openshift-ai-inference-authn`: `get` (template revision 5, Models audit below) |
| `openshift-ingress-operator` (`INGRESS_OPERATOR_NAMESPACE`) | IngressController `default`: `get` (template revision 5, Models audit below) |

**Cluster-wide (ClusterRole).** The operator namespaces (`redhat-ods-*`, `seaweedfs`) and the user's pipeline projects do not exist when the template is applied, so these rules are cluster-wide. Wherever the API allows it, they are limited to fixed object names. `create` cannot be limited by name.

| What | Verbs | Limited to names? |
|---|---|---|
| Secrets | `create` | no (pipeline projects are user-chosen) |
| Secrets | `get patch delete` | `seaweedfs-s3-credentials`; `nightly-dspa-s3` (`get delete patch`); the earlier storage's `minio-secret` and legacy `dashboard-dspa-secret` (`get delete`) |
| Namespaces | `get list create`; `patch`; `delete` | `patch` only `redhat-ods-operator`; `delete` only `minio` (the earlier storage's namespace, at the end of Migrate storage) and `seaweedfs` (at the end of the storage's Tear down), each only when the user ticks it, offered only when it carries the tool label and the tool's managers wrote it (UID precondition) |
| Deployments | `get list patch create`; `delete` | `delete` only `seaweedfs` (the S3 storage, and the SeaweedFS earlier versions ran in namespace `minio`) and `minio` (MinIO of earlier versions) |
| Services | `get create`; `patch delete` | `patch delete` only `seaweedfs-s3`; `delete` only `minio-service` (the earlier storage's) |
| PVCs | `list create`; `patch delete` | `patch delete` only `seaweedfs-data`; `delete` only `seaweedfs-pvc` and `minio-pvc` (the earlier storage's data) |
| Routes | `create`; `get` | `get` on `rhods-dashboard`, `mlflow`, `seaweedfs-admin`, `minio-api`, `minio-ui`; `patch delete` on `seaweedfs-admin`; `delete` on `minio-api`, `minio-ui` (the earlier storage's) |
| NetworkPolicies | `create`; `get patch delete` | the second set only on `seaweedfs-ingress` (the name in both storage namespaces); `get delete` on `minio-ingress` (MinIO's policy of earlier versions) |
| Pods, ReplicaSets, Events, EndpointSlices, Nodes | `list` | read-only |
| Subscriptions | `create`; `get patch delete` | the second set only on `rhods-operator` |
| OperatorGroups | `create list`; `patch` | `patch` only `rhods-operator` |
| ClusterServiceVersions | `get list delete` | no |
| InstallPlans | `get list patch delete` | no |
| Validating/Mutating webhook configurations | `list delete` | no |
| CRDs | `list`; `get patch` | `get patch` only the DSC and DSCI CRDs |
| DataScienceClusters | `get list patch create` | no |
| DSCInitializations, Platforms | `list` (`get` Platform `default`) | read-only |
| `components.platform.opendatahub.io/*` (module CRs) | `list patch` | no; the kinds change between releases and are discovered |
| DSPAs | `list create`; `get patch delete` | only `nightly-dspa` and legacy `dspa` (Migrate storage moves a `dspa` an earlier version created to the new storage) |
| Jobs | `create`; `get delete` | `get delete` only `seaweedfs-migrate`, Migrate storage's copy Job in namespace `seaweedfs` |
| MLflows | `create`; `get patch delete` | the second set only on `mlflow` |
| ClusterVersion, Console, IDMS, ConfigMap `odh-upgrade-acks` | read | — |
| `operator.openshift.io/*` (prerequisite operands such as `JobSetOperator/cluster`) | `get` | only objects named `cluster`; the kind comes from the prerequisite's CSV. This group also holds OpenShift's own operator configs (`console/cluster`, …), which become readable; they hold no secrets (template revision 4) |
| cert-manager `Certificates` | `list` | read-only; Certificates name Secrets but hold no key material (template revision 4) |
| MLflow `mlflow.kubeflow.org` experiments, datasets, registeredmodels (checked by MLflow, not Kubernetes objects) | `create`; `get update` on experiment `MLflow Demo`; `get update delete` on experiment `nightly-sample-training` and model `nightly-sample-classifier` | `create` cannot be; everything else only the samples' names (template revision 5, samples audit below) |
| MLflow `mcpservers`, `skills` | `get update delete` | only `com.deepwiki/rhoai-nightly-sample` and `nightly-sample-mcp-builder`; no `create` |
| DSPA `datasciencepipelinesapplications/api` (checked by the pipeline server's kube-rbac-proxy) | `get create delete` | only `nightly-dspa` |
| GatewayClasses | `create`; `get patch delete` | the second set only on `openshift-ai-inference` (template revision 5, Models audit below) |
| LLMInferenceServices | `create`; `get list`; `get patch delete` | the last set only on `qwen25-05b-instruct`, the catalog's model (template revision 5, Models and Playground audits below) |
| InferenceServices | `get list` | read-only; for the dashboard's gen-ai API acting as the SA (template revision 5, Playground audit below) |
| OGXServers (`ogx.io`) | `list create`; `get delete` | the second set only on `lsd-genai-playground` (template revision 5, Playground audit below) |
| ConfigMaps | `create`; `get update delete`; `get update`; `get` | create cannot be; the rest only on the playground's `llama-stack-config`, `ogx-office-mime-types`, `ogx-router-ca-bundle`, `ogx-trusted-ca-bundle`; `odh-trusted-ca-bundle`; `gen-ai-aa-custom-model-endpoints` (template revision 5, Playground audit below) |
| LLMInferenceServiceConfigs | `get` | no: the CPU preset's name is found at run time (template revision 5) |
| Ingress config `cluster`, ImageTagMirrorSets | `get`; `list` | read-only (template revision 5) |

There is no cluster-wide `list` or general `get` on Secrets. The SA cannot read arbitrary Secrets.

**Audit: template revision 5 (3.0.0), the S3 storage's new names.** The storage moved from the MinIO-era names in namespace `minio` to its own names in namespace `seaweedfs`. Storage an earlier version left in `minio` is only detected, read for the migration, and deleted.

| Rule | What it allows | Worst case if the SA token leaks | Why nothing narrower works |
|---|---|---|---|
| Secrets `get patch delete` on `seaweedfs-s3-credentials` (replaces `minio-secret` for these verbs) | read, change and delete the storage's S3 keys | read or change any Secret named `seaweedfs-s3-credentials` in any namespace: the same reach as the `minio-secret` rule it replaces, under a name only this tool uses | the namespace is created by the tool after the template is applied, so the rule cannot be a Role; RBAC cannot limit a ClusterRole rule to one namespace |
| Secrets on `minio-secret`: `patch` removed, `get delete` kept | read the earlier storage's keys (the migration gives the new storage the same keys) and delete it with that storage | narrower than before | `get` is needed to keep the keys, `delete` to clean up |
| Services `patch delete` on `seaweedfs-s3`; `minio-service` down to `delete` | apply and remove the storage's Service | change or delete a Service of that name anywhere; `minio-service` can no longer be changed, only deleted | as above |
| PVCs `patch delete` on `seaweedfs-data`; `seaweedfs-pvc` down to `delete` (next to `minio-pvc`) | apply and remove the storage's data volume, delete the earlier storage's | delete a PVC of one of those names anywhere, and its data: the reach of the earlier rules | as above; PVCs are still listed, never read by name |
| Routes `get patch delete` on `seaweedfs-admin`; `minio-api`, `minio-ui` down to `get delete` | the admin UI Route; read the earlier Routes' hosts (a pipeline server may use one, so teardown checks them) and delete them | expose or remove the admin UI Route of that name; the earlier Routes can no longer be changed | as above |
| Namespaces `patch` on `minio` removed | — | narrower | the tool no longer relabels the earlier namespace |
| Namespaces `delete` on `seaweedfs` (next to `minio`) | the storage's Tear down may also delete namespace `seaweedfs`, which the tool created: only when the user ticks "Also delete namespace seaweedfs", only after every storage object is gone, only when the namespace carries the tool label and a tool manager wrote it, with a UID precondition. The dialog says it deletes everything in the namespace and lists the Deployments and PVCs the tool sees there | delete a namespace named `seaweedfs` in the cluster, with everything in it: the S3 storage and its data (pipeline artifacts), which the token could already delete object by object (`delete` on the Deployment, Secret and PVC), plus anything else someone put there. It mints no access | `delete` is limited to the one name; a namespace cannot be a Role's own resource, and the tool creates it after the template is applied |

No verb was added on any resource type; every new name replaces an old one, and the old names keep only `get` and `delete` where the migration and teardown need them.

**Audit: template revision 5 (3.0.0), Migrate storage.** Moving the earlier storage's data and pipeline servers to the new storage, and removing the old storage afterwards, needs three grants.

| Rule | What it allows | Worst case if the SA token leaks | Why nothing narrower works |
|---|---|---|---|
| Jobs (`batch`) `create` | start the copy Job `seaweedfs-migrate` (rclone, pinned by digest; `RCLONE_IMAGE` overrides it) in namespace `seaweedfs` | create a Job with any image in any namespace. Its pods run as that namespace's `default` ServiceAccount under the namespace's SCC admission, so they gain no API access the token does not already have; this is the reach of the Deployment `create` the SA already holds | `create` cannot be limited by name (the collection POST carries none), and the namespace is created by the tool, so a Role cannot hold it. The copy has to run inside the cluster network: the backend could only stream every object through its own pod |
| Deployments `list` in `minio` (existing cluster-wide grant) | name the Deployments the tool did not create, before the user decides to delete the namespace | none new | — |
| Jobs `get delete` on `seaweedfs-migrate` | read the Job's state and delete it (by UID) once its result is read | read or delete a Job of that name in any namespace | as above; the result comes back in the pod's termination message, read with the existing pod `list`, so no `pods/log` grant is added |
| DSPAs `patch` on `dspa` (next to `nightly-dspa`) | move a pipeline server an earlier version created to the new storage, and label it | change any DSPA named `dspa`, which is also the name odh-dashboard gives the pipeline server it creates. The code patches one only when its name, the old storage's address and its secret name match and the tool's field managers (`rhoai-nightly-updater`, or `Go-http-client` of the earliest versions) own its `spec.objectStorage.externalStorage` host and secret name (`dspaInfo.managedByTool`). The dashboard writes as no manager (its Node backend sends no User-Agent) or as `Mozilla` (its Go BFF forwards the browser's), so its DSPAs fail that test. The patch carries a `resourceVersion` precondition; other DSPAs are listed for the user, never changed | RBAC cannot tell who created an object; the older versions used this name |
| Namespaces `delete` on `minio` | delete namespace `minio` after Migrate storage removed the old storage, when the user ticks it (unticked by default); offered only when the namespace carries the tool label and the tool's managers wrote it (UID precondition) | delete a namespace named `minio` and everything in it | the name is fixed. Who created a namespace cannot be proven from its metadata (a managedFields entry's time is the manager's last write), so this is the user's informed choice: the dialog says it deletes everything in the namespace, including what the tool did not create, and lists the Deployments and PVCs there that the tool did not create |

The copy Job's pod: `runAsNonRoot`, `allowPrivilegeEscalation: false`, all capabilities dropped, seccomp `RuntimeDefault`, no UID (restricted-v2 assigns one), `automountServiceAccountToken: false`, CPU and memory requests and limits, `backoffLimit: 0`, `activeDeadlineSeconds: 3600`, `ttlSecondsAfterFinished: 86400`. The S3 keys reach it only through `secretKeyRef` to `seaweedfs/seaweedfs-s3-credentials`, never in its command, arguments or the operation's output. It needs no NetworkPolicy change: `seaweedfs-ingress` admits the S3 port from every pod, and so do the earlier storage's policies.

**Audit: template revision 5 (3.0.0), samples.** The Environment page adds sample data to one project: four MLflow samples (MLflow's own GenAI demo, a training experiment with a registered model, an MCP server entry, an agent skill) and a sample pipeline run. These calls go to MLflow (at its CR's `status.url`) and to the project's pipeline server, not to the Kubernetes API, with the app's own token (the SA's in the pod, `DEV_TOKEN` in DEV_MODE). Both servers authorize that token with access reviews:

- MLflow (mlflow-kubernetes-plugins 2.1.1, SelfSubjectAccessReview mode) checks group `mlflow.kubeflow.org` in the namespace the `X-MLflow-Workspace` header names, first without a name and, when that is denied, with each resolved name (`auth/core.py:812-851`). So `resourceNames` limit these rules; `create` is never checked with a name (`auth/authorizer.py:131-140`).
- The DSPA's kube-rbac-proxy checks `datasciencepipelinesapplications/api` on the DSPA's own name, with the verb taken from the HTTP method (DSPO `kube-rbac-proxy-config.yaml.tmpl:11-17`; opendatahub-io/kube-rbac-proxy `pkg/proxy/proxy.go:73-89`).

The routes are mutations behind the cluster-admin gate, the lock and the activity log; the two status reads use only named reads. `pkg/cluster/samples_rbac_test.go` derives these rules from the code's calls (the test MLflow and KFP servers serve only the listed routes and decide every review with this template), so a missing or unused rule fails the tests.

| Rule | What it allows | Worst case if the SA token leaks | Why nothing narrower works |
|---|---|---|---|
| `mlflow.kubeflow.org` experiments, datasets, registeredmodels: `create` | MLflow's demo generator (`POST /ajax-api/3.0/mlflow/demo/generate` checks all three without a name, `rules_base.py:1178-1182`); creating the training experiment and the registered model | create MLflow experiments, evaluation datasets, registered models or prompts in any project's MLflow workspace: clutter. It reads nothing and changes no existing entity | `create` cannot be limited by name, in Kubernetes RBAC or in the plugin |
| experiments `get update` on `MLflow Demo` | read the demo's state; restore it after a removal in MLflow and set its feature version tags to `0`, so `demo/generate` regenerates every feature (the fix for the incomplete demo after MLflow's own removal) | read the demo experiment of any project and change it: log runs into it, change its tags, restore it | named; the name is MLflow's constant (`demo/base.py:10`). There is no `delete`: the tool cannot remove the demo (MLflow's `demo/delete` checks `delete` on all three types without a name, `rules_base.py:1183-1187`, and that right is not granted) |
| experiments `get update delete` on `nightly-sample-training`; registeredmodels `get update delete` on `nightly-sample-classifier` | the training sample: runs, metrics, model versions, the `champion` alias, and its removal (model deleted, experiment soft-deleted) | read, change or delete an experiment or registered model of those names in any project | named. The tool also refuses to touch an object of these names that lacks its marker tag `rhoai-nightly-updater.sample` |
| mcpservers `get update delete` on `com.deepwiki/rhoai-nightly-sample` | register the MCP sample (a version POST auto-creates the server and is checked as `update` on its name, `rules_base.py:1351-1354`), deprecate and delete it | add, change or delete an MCP registry entry of that name in any project. It points at DeepWiki's public server; a changed entry could point the Gen AI playground elsewhere, but only for users who pick that entry | named; no `create` needed |
| skills `get update delete` on `nightly-sample-mcp-builder` | register the skill sample (a version POST is `update` on the parent, `rules_skill_registry.py:30-33`) and delete it | add, change or delete a skill of that name in any project (metadata only: MLflow never fetches the source) | named; no `create` needed |
| `datasciencepipelinesapplications/api` `get create delete` on `nightly-dspa` | the sample pipeline: create its KFP experiment, upload its pipeline, start runs, read their state, delete them | the full KFP API of every pipeline server named `nightly-dspa` (the tool's): run any pipeline (any image, as the project's `pipeline-runner-nightly-dspa` SA), read and delete runs, pipelines and experiments there. The SA can already create Deployments and Jobs with any image in any namespace, so no new code-execution reach | kube-rbac-proxy checks exactly this subresource and name; the verbs are the HTTP methods used (`update`, `patch` are not). `dspa`, the name odh-dashboard gives its pipeline servers, is not included: samples run only on the tool's own servers |

No cluster-wide `delete` (or `get`, `update`, `list`) on MLflow experiments, datasets or registered models is granted, and no Kubernetes object becomes readable or writable: `mlflow.kubeflow.org` resources exist only as names in MLflow's access reviews. The gitops profile refuses the samples routes and `deploy/rbac-gitops.yaml` gets none of these rules. Leftovers: MLflow keeps soft-deleted experiments and runs (and the demo's REJECTED issues) until its garbage collection runs; KFP keeps a run's artifacts in S3 and its MLMD records.

Check on a cluster (replace `<ns>` with the app's namespace and `<sa>` with its ServiceAccount, by default `rhoai-nightly-updater`). `mlflow.kubeflow.org` is not an API the server serves, so `oc auth can-i` cannot resolve it (it answers `no` even for allowed rules); ask the API server with a SubjectAccessReview, as MLflow does. Verified on a 3.6 cluster on 2026-11-17: the six "yes" and six "no" cases below gave exactly these answers.

```bash
SA=system:serviceaccount:<ns>:<sa>; P=<a project>
sar() {  # sar <verb> <resource> [<name>] -> true/false, as MLflow's access review sees it
  oc create -o jsonpath='{.status.allowed}{"\n"}' -f - <<EOF
{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"$SA",
 "groups":["system:serviceaccounts","system:serviceaccounts:<ns>","system:authenticated"],
 "resourceAttributes":{"namespace":"$P","group":"mlflow.kubeflow.org","resource":"$2","verb":"$1","name":"${3:-}"}}}
EOF
}
sar create experiments                                    # true
sar update experiments 'MLflow Demo'                      # true
sar delete experiments nightly-sample-training            # true
sar delete experiments 'MLflow Demo'                      # false
sar delete experiments                                    # false
sar delete registeredmodels some-other-model              # false
# The pipeline server's proxy checks a real subresource, so oc auth can-i works there:
oc auth can-i create datasciencepipelinesapplications.datasciencepipelinesapplications.opendatahub.io/nightly-dspa --subresource=api -n $P --as=$SA  # yes
oc auth can-i get datasciencepipelinesapplications.datasciencepipelinesapplications.opendatahub.io/dspa --subresource=api -n $P --as=$SA           # no
```

**Audit: template revision 5 (3.0.0), Models.** The Environment page's Models tab sets up the cluster's inference gateway (GatewayClass `openshift-ai-inference`, ConfigMap `openshift-ingress/openshift-ai-inference-options`, Gateway `openshift-ingress/openshift-ai-inference`) and a small CPU model (LLMInferenceService `qwen25-05b-instruct` in a data science project), and removes them. The Gateway and its ConfigMap live in OpenShift's own ingress namespace, which exists on every cluster, so their rules are a Role there, not cluster-wide. Nothing grants access to a model: no ServiceAccount, Role, RoleBinding or token is created. The gateway's AuthPolicy (made by odh-model-controller) accepts any token whose user may `get` the model, so the "Use it" snippets use the user's own `oc whoami -t`.

| Rule | What it allows | Worst case if the SA token leaks | Why nothing narrower works |
|---|---|---|---|
| GatewayClasses `create` (cluster-wide) | create GatewayClass `openshift-ai-inference` (OpenShift's controller `openshift.io/gateway-controller/v1`) when the cluster has none | create GatewayClasses of any name for any controller. A class alone runs nothing: a Gateway must use it, and the token can create Gateways only in `openshift-ingress` (below) | `create` cannot be limited by name; GatewayClasses are cluster-scoped |
| GatewayClasses `get patch delete` on `openshift-ai-inference` | read it (whose controller it names), apply and delete the tool's own | change (`spec.parametersRef`, `description`; `controllerName` is immutable) or delete the class of that name, also one that RHOAI's chart or an admin made: the code refuses those, the token could not be stopped. Deleting it breaks every Gateway of that class, so models behind it stop answering (a denial of service; the class keeps a finalizer while Gateways use it) | named; no access is minted |
| `openshift-ingress` Gateways `create` | create Gateway `openshift-ai-inference` | create any Gateway in `openshift-ingress`. OpenShift then provisions a cloud load balancer for it (cost), and the ingress operator publishes its listeners' hostnames in the cluster's DNS zones. A listener for an existing `*.apps` hostname (for example the console's), terminated with a certificate Secret in that namespace that the Gateway names (the router's default certificate), would take that hostname's traffic away from the router: a denial of service of that hostname. It could not read the traffic's content beyond what reaches the new gateway, and no route sends it anywhere unless one attaches (the token can create no HTTPRoute; an LLMInferenceService's routes cover only its own paths) | `create` cannot be limited by name; the namespace is the gateway's fixed home, so the rule is a Role there, not cluster-wide |
| `openshift-ingress` Gateways `get patch delete` on `openshift-ai-inference` | read its status, apply and delete the tool's own | change or delete the inference gateway, also one RHOAI's chart or an admin made (the code never does): the hostname takeover above for this one Gateway, or every model behind it unreachable | named, in one namespace |
| `openshift-ingress` ConfigMaps `create`; `get patch delete` on `openshift-ai-inference-options` | the gateway's options: the gateway controller merges its `service` key into the Gateway's Service, so service-ca issues the in-cluster listener's certificate | create ConfigMaps in `openshift-ingress` (an existing name cannot be overwritten by `create`; nothing there reads an arbitrary new name), and change the options of the inference gateway: for example its Service's type or annotations, changing how the gateway is exposed | `create` cannot be limited by name; the rest is named, in one namespace |
| `openshift-ingress` DNSRecords `list` | whether the external hostname is published in DNS (status only) | read the hostnames and load balancer targets the ingress operator publishes: no secrets | DNSRecords are found by their `spec.dnsName`; their names are generated |
| `openshift-ingress` AuthPolicy `get` on `openshift-ai-inference-authn` | warn when the gateway's authentication is missing or not enforced | read that policy (its rules, no secrets) | named, in one namespace |
| `openshift-ingress-operator` IngressController `get` on `default` | the name of the default certificate's Secret, which the external listener names (the Secret itself is never read) | read the default IngressController: domain, replicas, certificate name; no secrets | named, in one namespace |
| Ingress config `get` on `cluster` | the apps domain, for the hostname `inference.<apps domain>` | read the cluster's apps domain, which every Route shows anyway | named |
| ImageTagMirrorSets `list` (with the existing ImageDigestMirrorSets `list`) | whether a mirror serves the CPU preset's image repository, which decides between the preset's image and the newest released one on nightly clusters | read the cluster's mirror configuration: registry hostnames, no credentials | mirror sets have no fixed names |
| LLMInferenceServices `list` (cluster-wide; since the Playground audit in one rule with `get`, below) | the Models tab's status across projects, and the gateway's removal guard: the gateway is not deleted while any model in any namespace routes through it (fails closed when the list cannot be read); the Gateway's own count of attached routes (`status.listeners[].attachedRoutes`, read with the existing `get`) also stops it, which catches models that inherit their route from a preset and routes made by hand | read every LLMInferenceService's spec: model URIs, images, resources, and environment variables written inline in `spec.template` (a token written there as a plain value instead of a Secret reference would be exposed) | which models use the gateway can only be known by reading them all; RBAC cannot limit `list` by label |
| LLMInferenceServices `create` (cluster-wide) | create the model in a project the user chooses | create an LLMInferenceService in any namespace, which runs a pod with any image (`spec.template`) as that namespace's KServe workload: the same code-execution reach as the Deployment `create` the SA already holds | `create` cannot be limited by name, and projects are user-chosen and made after the template is applied |
| LLMInferenceServices `get patch delete` on `qwen25-05b-instruct` | read, apply (repair, or a switch to the newest released image) and delete the tool's model | change or delete any model of that name in any namespace, also one made in the dashboard (the code refuses those: label plus the tool's field manager, UID-preconditioned delete); a patched image runs as code, as with Deployments | named; the catalog is a fixed list in the code (`modelCatalog`) |
| LLMInferenceServiceConfigs `get` (cluster-wide) | read the CPU preset (`<LLM_INFERENCE_SERVICE_CONFIG_PREFIX>config-llm-template-cpu`) to check that it exists and find its image | read any LLMInferenceServiceConfig (RHOAI's presets and users' local copies): templates and images, no secrets by design | the preset's name is derived at run time from the controller's prefix (release-specific, e.g. `v3-6-0-kserve-`), so it cannot be named; the applications namespace may not exist when the template is applied |
| HardwareProfiles (`infrastructure.opendatahub.io`) `list` (cluster-wide) | find the dashboard hardware profile that fits the model, and record it on the model as the dashboard's deploy wizard does, so its Deployments page names the profile | read every hardware profile: names, CPU, memory and accelerator ranges, node selectors and tolerations. No secrets; profiles are visible to every dashboard user | the profiles live in the applications namespace, which is only known at run time (as for LLMInferenceServiceConfigs); RBAC cannot limit `list` by name |

Reads that need no new rule: the controller's prefix (Deployment `llmisvc-controller-manager`, existing `get` on Deployments), the model's pods and events (existing `list`), node capacity (existing `list` on nodes and pods), the gateway's Service (existing `get` on Services), the Subscription (whether the cluster runs a nightly build) and the DataScienceCluster. The released image is found with one anonymous HTTPS request to Red Hat's public container catalog (`catalog.redhat.com`, no credentials sent), only during a setup that needs it, and is pulled from `registry.redhat.io` with the cluster's own pull secret; `VLLM_CPU_IMAGE` replaces the lookup on disconnected clusters. The route `POST /api/resources/models/stream` is a mutation behind the cluster-admin gate, the lock, the rate limit and the activity log; the gitops profile refuses it, and `deploy/rbac-gitops.yaml` gets none of these rules. Check on a cluster:

```bash
SA=system:serviceaccount:<ns>:<sa>
oc auth can-i create gateways.gateway.networking.k8s.io -n openshift-ingress --as=$SA                       # yes
oc auth can-i delete gateways.gateway.networking.k8s.io/openshift-ai-inference -n openshift-ingress --as=$SA # yes
oc auth can-i delete gateways.gateway.networking.k8s.io/maas-default-gateway -n openshift-ingress --as=$SA   # no
oc auth can-i create gateways.gateway.networking.k8s.io -n default --as=$SA                                 # no
oc auth can-i delete llminferenceservices.serving.kserve.io/qwen25-05b-instruct -n <project> --as=$SA        # yes
oc auth can-i delete llminferenceservices.serving.kserve.io/other-model -n <project> --as=$SA                # no
oc auth can-i list hardwareprofiles.infrastructure.opendatahub.io -n redhat-ods-applications --as=$SA      # yes
oc auth can-i create hardwareprofiles.infrastructure.opendatahub.io -n redhat-ods-applications --as=$SA    # no
oc auth can-i get secrets/router-certs-default -n openshift-ingress --as=$SA                                # no
```

**Audit: template revision 5 (3.0.0), Playground.** The Environment page's Playground tab sets up and removes the RHOAI dashboard's Gen AI playground in a project by calling the installed dashboard's own gen-ai API (`/gen-ai/api/v1/aaa/models`, `maas/models`, `lsd/status`, `lsd/install`, `lsd/delete` on the dashboard's address, `https://rh-ai.<apps domain>`), exactly as a click in the dashboard's Gen AI studio does. The dashboard then writes whatever this RHOAI release's playground is made of; no playground configuration is copied into this tool.

*Whose token.* The calls carry the app's ServiceAccount token (`DEV_TOKEN` in dev mode), chosen in one place (`pkg/api` `genAIToken`). The user's session token is not used: oauth-proxy's session token keeps its default scope `user:info user:check-access`, which identifies the user and runs access reviews but cannot create anything, and widening it (`--scope`) would put a full token in every session cookie. `TestTemplateOAuthProxyKeepsTheDefaultScope` fails if the template sets `--scope`. The user's token still decides who may act: `POST /api/resources/playground/stream` is a mutation behind the cluster-admin gate, the lock, the rate limit and the activity log, and the gitops profile refuses both playground routes (`deploy/rbac-gitops.yaml` gets none of these rules). The status route (`GET /api/resources/playground`) is a read for any signed-in user, like the other Environment status reads.

*Data flow of the token.* The SA token goes only to the dashboard's gen-ai API on this cluster: the host is the address the cluster's own Dashboard component reports (`status.url`, else Route `rhods-dashboard`), https only, verified against the system roots and the pod's CA bundles (the same client as the MLflow samples, `samplesTLSConfig`). It is sent only in the `Authorization` header; it is never logged, put in an error, a step message, a response or the activity log (the playground tests check every message for it). The data-science gateway in front of the dashboard validates it with a TokenReview (GatewayConfig `enableK8sTokenValidation`), and the dashboard's API then runs one access review (`list ogxservers` in the project) and acts with the token.

*Why these rules.* The dashboard's API acts with the caller's rights, so the SA needs what the dashboard's install and delete do in the user's project. These rules were found live on RHOAI 3.6 as the minimum: a full cycle install, Ready, chat, delete (nothing left) and re-install passed with no permission error. The pgvector database and the copy of the router CA are made by the dashboard's own ServiceAccount, not with this token. `pkg/cluster/playground_rbac_test.go` lists each request the dashboard makes and the coverage test checks the template against it.

| Rule | What it allows | Worst case if the SA token leaks | Why nothing narrower works |
|---|---|---|---|
| OGXServers `list` (cluster-wide) | the dashboard API's only gate: an access review "may list ogxservers in the project", on every call; the install's check for an existing playground | read every OGXServer's spec: server configuration, images, environment variables written inline (a secret written there as a plain value instead of a Secret reference would be exposed) | the gate asks for `list` in whatever project is chosen; projects are made after the template is applied |
| OGXServers `create` (cluster-wide) | the install creates OGXServer `lsd-genai-playground` | create an OGXServer in any namespace, which the OGX operator turns into a Deployment running the image the spec names: the same code-execution reach as the Deployment `create` the SA already holds | `create` cannot be limited by name |
| OGXServers `get delete` on `lsd-genai-playground` | the install's ownerReferences and checks; the dashboard's delete | delete the playground of any project, also one a user made in the dashboard (the code refuses those: it deletes only where this tool recorded its own request), losing its chat history and uploaded documents | named |
| ConfigMaps `create` (cluster-wide) | the install creates the playground's configuration ConfigMaps | create a ConfigMap of a new name in any namespace (an existing one cannot be overwritten by `create`). This adds little beyond the cluster-wide Secret and Deployment `create` the SA already holds; a workload that mounts a ConfigMap by a name that does not exist yet could be fed content | `create` cannot be limited by name; the dashboard creates these in the user's project |
| ConfigMaps `get update delete` on `llama-stack-config`, `ogx-office-mime-types`, `ogx-router-ca-bundle`, `ogx-trusted-ca-bundle` | the install sets an ownerReference to the OGXServer on each, which needs `delete` on the object ("cannot set an ownerRef on a resource you can't delete"); without it they are orphaned on delete and the next install fails "already exists" | change or delete the playground configuration of any project: point a playground at another model endpoint or CA bundle (its chat traffic and the documents users upload could go elsewhere), or break it | named; these are the playground's own names |
| ConfigMaps `get update` on `odh-trusted-ca-bundle` | the install adds the label `ogx.io/watch=true` to the project's CA bundle, which the OGX operator's cache needs (without it the server fails and chat answers 500) | change the trusted CA bundle of any namespace, which every RHOAI workload there mounts: adding a CA would let someone who also controls the network intercept those workloads' TLS. **Live evidence (RHOAI 3.6, 2026-11-16):** the RHOAI operator owns this ConfigMap (DSCInitialization trusted CA bundle) and restored a tampered copy's data within about 1 s (the write was accepted, the content reverted before the next read), so the window is about a second per write; the label survives because the operator does not manage labels | named; the dashboard adds only a label, but RBAC cannot limit an update to labels |
| ConfigMaps `get` on `gen-ai-aa-custom-model-endpoints` | the dashboard's model list reads this optional, user-managed ConfigMap of custom endpoints | read custom model endpoints a user listed (URLs; API keys are kept in Secrets) | named |
| InferenceServices `get list`, LLMInferenceServices `get` (cluster-wide; `list` existed) | the model list (`aaa/models`) and the install's check of each model's endpoint | read every InferenceService and LLMInferenceService: model URIs, images, resources and inline environment variables (as for the existing LLMInferenceService `list`) | the dashboard lists the project's models and reads each one; projects are user-chosen |

Reads that need no new rule: the DataScienceCluster (the OGX component and its readiness condition), Deployment `gen-ai-ui` (existing `get` on Deployments), namespaces, and this tool's own snapshot ConfigMap, where it records the projects it requested a playground in (data key `playgrounds`, written by server-side apply under its own field manager with the existing `patch` on that ConfigMap). Check on a cluster:

```bash
SA=system:serviceaccount:<ns>:<sa>
oc auth can-i list ogxservers.ogx.io -n <project> --as=$SA                                   # yes
oc auth can-i delete ogxservers.ogx.io/lsd-genai-playground -n <project> --as=$SA            # yes
oc auth can-i delete ogxservers.ogx.io/other -n <project> --as=$SA                           # no
oc auth can-i delete configmaps/llama-stack-config -n <project> --as=$SA                     # yes
oc auth can-i delete configmaps/odh-trusted-ca-bundle -n <project> --as=$SA                  # no
oc auth can-i update configmaps/odh-trusted-ca-bundle -n <project> --as=$SA                  # yes
oc auth can-i update configmaps/other -n <project> --as=$SA                                  # no
oc auth can-i list inferenceservices.serving.kserve.io -n <project> --as=$SA                 # yes
oc auth can-i delete inferenceservices.serving.kserve.io/any -n <project> --as=$SA           # no
oc get deploy -n <ns> <app> -o yaml | grep -- '--scope'                                       # nothing
```

**Residual risk.** The SA can still create Secrets anywhere, create Subscriptions (OLM then installs whatever the CSV requests), delete CSVs and webhook configurations, and patch any Deployment (the module-operator restart uses this existing grant; the API cannot limit `patch` to Deployments it discovers at run time without listing every module operator by name). Treat its token like an administrator credential: anyone who can read it (or `exec` into the pod) can act as the SA. Namespace `admin` on the app's namespace is therefore effectively privileged.

## Network exposure

| Port | Where | Who can reach it |
|---|---|---|
| 8080 | app, bound to `127.0.0.1` (`BIND_ADDRESS`) | only oauth-proxy in the same pod. The API is never exposed on the pod IP |
| 8443 | oauth-proxy, Service port `https`, Route (TLS `reencrypt`) | NetworkPolicy: only from the ingress (router) namespaces |
| 9090 | app, `METRICS_PORT`: `/api/health`, `/api/health/ready`, `/api/version`, `/metrics` only | NetworkPolicy: only from `openshift-monitoring` and `openshift-user-workload-monitoring`. Kubelet probes are node traffic and always allowed |

## Sessions and CSRF

- The cookie secret is in Secret `<APP_NAME>-proxy` (key `session_secret`), mounted with `--cookie-secret-file`. Re-applying the template keeps it, so sessions survive upgrades. `make deploy`/`make upgrade` create it once.
- Cookies use `--cookie-expire=23h`, below the default OAuth access-token lifetime (24h), and `SameSite=Strict; Secure; HttpOnly`. An expired session shows a "Sign in again" banner.
- The Deployment's `revisionHistoryLimit` is 2. After a successful rollout, `make deploy`/`make upgrade` delete the app's old ReplicaSets: those created before the cookie Secret held the secret in plaintext env.
- State-changing `/api/` requests must be `Content-Type: application/json` (otherwise 415). A browser cannot send that cross-site without a CORS preflight, and the backend never grants one.
- Every response carries CSP, `X-Frame-Options: DENY`, `nosniff`, HSTS and `Cache-Control: no-store` (pkg/middleware/security.go).

## Abuse limits and input validation

- Mutations: one accepted request per user and endpoint per 30 s, taken when the request is accepted. Rejected requests (4xx/5xx, including 409 busy) and dry runs do not use the slot. One cluster operation at a time (409 `cluster_busy`). A 15-minute deadline applies, independent of the browser.
- Mutation bodies are capped at 4096 bytes (`/api/pageview` at 256; the clean-up's list of at most 200 projects at 16 KiB), and the read timeout is 15 s.
- **Clean up** (Environment page, `POST /api/resources/cleanup/stream`) is one operation: one cluster lock and one rate-limit slot for the whole run, the same cluster-admin gate and 15-minute deadline as every mutation. It needs no new RBAC: each part runs the part's own teardown (pipeline servers, then the S3 storage, then MLflow, then models, then the inference gateway) with its permissions and guards, so it deletes only what this tool created, never a storage a pipeline server still uses, never a gateway a model still routes through, and namespace `seaweedfs` only when asked and only when this tool created it.
- **Models** (Environment page, `POST /api/resources/models/stream`) is one operation per Set up or removal, with the same gate, lock, rate limit and 15-minute deadline; the model's wait ends 30 seconds before that deadline. The body is capped at 4096 bytes; the project is validated (DNS-1123) and must be a data science project; the model must be one of the catalog's. Setup checks every object it writes and dry-runs every write before the first real one, so a refusal changes nothing. Project names are validated (DNS-1123, no duplicates) before the lock is used; the first part that does not succeed stops the run.
- **Feature prerequisites** (Environment page): the status reads the DataScienceCluster's `spec.components` / `status.components` management state with the `get`/`list` on `datascienceclusters` that `deploy/template.yaml` already grants (the Components page uses it), and setup refuses on the same reading. No new RBAC.
- Images must match `quay.io/rhoai/` or `registry.redhat.io/rhoai/`. A custom build to install accepts only `quay.io/rhoai/rhoai-fbc-fragment` with a tag and/or digest. PR numbers, DSC names, channels and namespaces are validated before use. The static file server is protected against path traversal.
- `/api/pageview` stores only aggregate counters, with labels limited to known page/feature names. No user identity is stored.

## Credentials

- The pull-secret token is never logged, returned, or written to the activity log. Only the `quay.io/rhoai` entry of `kube-system/additional-pull-secret` is replaced; other registries are kept.
- The Quay token is derived from the cluster pull secret on the server and never sent to the browser.
- The S3 storage's access key and secret key are random per install (or set with `SEAWEEDFS_ACCESS_KEY_ID` and `SEAWEEDFS_SECRET_ACCESS_KEY` on a first setup) and stored in `seaweedfs/seaweedfs-s3-credentials`. The secret key is also the SeaweedFS admin UI password; it reaches the pod through the environment (`WEED_ADMIN_PASSWORD`), not the command line.
- **The S3 keys** (Environment page, Pipelines tab, S3 storage: the access key and the secret key, which is also the admin UI password, shown or copied on demand, and "Copy as environment variables") come from `GET /api/resources/minio/admin-password`. It needs no new RBAC: the ServiceAccount already has `get` on Secret `seaweedfs-s3-credentials` (by `resourceNames`) for setup and Migrate storage. The handler reads the Secret only on that request, never in the status; it answers only a user who passes the cluster-admin check (such a user can read the Secret with their own token, so nothing is revealed that they could not read), only for storage this tool created (tool-labelled namespace, Secret written by the tool), with `Cache-Control: no-store`. The log line names the user, never the value. It takes no lock and changes nothing. The gitops profile has no Environment page, so the route answers 403 `profile_gitops` there, before any read (`deploy/rbac-gitops.yaml` grants no Secret read).
- `GITHUB_TOKEN` (optional) is sent only to `api.github.com`. Use a read-only token with public-repository access.

## Local development (`DEV_MODE=true`)

- Used by `dev.sh`. With no mounted SA token, every request runs with your own `oc` token (`DEV_TOKEN`) and **there is no cluster-admin gate**.
- The server binds `127.0.0.1` by default. A non-loopback `BIND_ADDRESS` exits at startup unless `DEV_ALLOW_REMOTE=true` (not recommended: anyone who can reach it acts with your token).
- Requests whose `Host` or `Origin` is not this machine on the API port or `DEV_FRONTEND_PORT` (default 9000) are refused. This blocks DNS rebinding.
- API server TLS is verified, in this order: `KUBE_CA_FILE` if set (an unusable file fails closed), the in-pod SA CA, then the system trust store. Verification is skipped only with `DEV_MODE=true` **and** `DEV_INSECURE_TLS=true`, which logs a warning.
- DEV_MODE cannot weaken an in-cluster pod. The bypass requires that no SA token be mounted.

## Known limitations and residual risks

- **SA token.** See *Residual risk* above.
- **S3 storage (SeaweedFS).** The Environment page's S3 storage runs SeaweedFS 4.54 (`ghcr.io/chrislusf/seaweedfs`, Apache-2.0, pinned by digest), the object store upstream Kubeflow Pipelines uses by default. It replaced the bundled MinIO, whose open-source line ended (advisories such as GHSA-hv4r-mvr4-25vw are fixed only in the commercial AIStor). SeaweedFS is not supported by Red Hat, publishes advisories often (bump the digest when one applies), and `weed mini` opens many listeners: master, volume and filer HTTP/gRPC, Iceberg and Lance, plus the admin's worker gRPC on loopback, and the master and filer HTTP APIs need no authentication. Mitigations:
  - NetworkPolicy `seaweedfs-ingress`: only the S3 port 8333 from pods in any namespace and the admin UI port 12535 only from the router; every other port is unreachable from the pod network. The admin's compatibility listener on the old port 23646 is turned off (`-admin.port.legacy=0`);
  - no Route to the S3 API;
  - credentials random per install; the admin UI requires a login (user `admin`, password from the Secret);
  - the admin password is passed as an environment variable, not an argument, so `ps` does not show it. Anyone who can `oc exec` into the pod or read `seaweedfs/seaweedfs-s3-credentials` can still read it.
  - logs: at startup SeaweedFS logs the S3 access key ID once ("Added admin identity from AWS environment variables: name=…, accessKey=…", `weed/s3api/auth_credentials.go:611` at 4.54). The access key ID is an identifier, not a secret, like an AWS access key ID; the secret key and the admin password are not logged (checked live on 4.48; the logging code is unchanged in 4.54). The pinned version could avoid that line only by loading the identity from an `-s3.config` JSON file (identities are then logged at verbosity 2 only), which would put the secret key into a second, tool-generated Secret; that is not worth it for an identifier.

  Any pod in the cluster that knows the access key can still reach the S3 API. The storage is meant for test data only. Storage an earlier version left in namespace `minio` (SeaweedFS behind the MinIO-era names, or MinIO itself) keeps running, with its own policies, until it is migrated or torn down.
- **Restarts.** A running operation delays a pod restart by up to ~17 minutes (see [README: cluster safety](README.md#how-the-tool-keeps-your-cluster-safe)). A SIGKILL or node loss still interrupts it. There is no durable job queue; the next start reports the interrupted operation. A replacement pod that starts while the old one drains refuses changes until the old operation ends (lease in the operation ConfigMap, no new RBAC).
- **Mirror visibility.** The GitHub mirror contains team-internal links (Bitwarden collection, Slack channels, the IPA realm and AWS SAML alias). They are access-controlled, but internal. Decide with the team whether the mirror should be private.
