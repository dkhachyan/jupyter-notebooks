# notebook-route-controller

A Kubernetes controller that exposes Kubeflow Notebooks through Gateway API
HTTPRoutes on Envoy Gateway, replacing the Istio VirtualServices and
AuthorizationPolicies that the upstream setup relies on. It runs next to the
**unmodified** upstream notebook-controller (`USE_ISTIO=false`). It never
writes to Notebooks, StatefulSets or Services.

Deployment, configuration and the overall architecture are documented in the
Helm chart: [`charts/kubeflow-notebooks-no-istio`](../charts/kubeflow-notebooks-no-istio/README.md).

## What it manages

For every `kubeflow.org/v1` Notebook it server-side applies (field manager
`notebook-route-controller`), owned by the Notebook:

| Object | Name | Content |
|---|---|---|
| `gateway.networking.k8s.io/v1` HTTPRoute | `notebook-<ns>-<name>` | `PathPrefix /notebook/<ns>/<name>` → Service `<name>:80`; URL rewrite from `notebooks.kubeflow.org/http-rewrite-uri`; headers from `notebooks.kubeflow.org/http-headers-request-set`; removes `--remove-request-headers` (default `authorization`) |
| `gateway.envoyproxy.io/v1alpha1` SecurityPolicy | `notebook-<ns>-<name>` | Fully self-contained policy: `extAuth.http` pointing at the external authorization service (`--ext-auth-service-*`, headers forwarded via `headersToBackend`), the JWT provider (`--jwt-provider`) validating the forwarded `Authorization: Bearer` token, and `authorization.defaultAction: Deny` + allow rules for the users/groups allowed by RBAC |
| `networking.k8s.io/v1` NetworkPolicy (optional) | `notebook-<ns>-<name>` | Only `--network-policy-peers` may reach the notebook container port; other ports unrestricted |

Because a SecurityPolicy in the notebook's namespace references a Service in
the ext-auth namespace, the controller also maintains one cross-namespace
object per notebook namespace (not owned by the Notebook, apply-on/get-off):

| Object | Name | Content |
|---|---|---|
| `gateway.networking.k8s.io/v1beta1` ReferenceGrant | `notebook-extauth-<ns>` | In the ext-auth namespace (`--ext-auth-service-namespace`): lets a `gateway.envoyproxy.io SecurityPolicy` from namespace `<ns>` reference the `Service --ext-auth-service-name` |

It is created with the first Notebook in a namespace and deleted after the
last one; a ReferenceGrant with the same name that the controller did not
create (missing managed-by label) is left untouched (event `ApplyFailed`).

The access list comes from SubjectAccessReviews (see `internal/access`). The
controller only reads RBAC objects to find candidate subjects. The API server
makes every allow/deny decision.

The controller refuses to modify an existing object with the same name that
is not controlled by the Notebook (event `ApplyFailed`).

## Jupyter web app namespaces API

Because the Jupyter web app's own backend answers `GET /jupyter/api/namespaces`
with every namespace of the cluster (its SubjectAccessReview needs cluster-wide
`list namespaces`), the controller can serve that endpoint itself when
`--namespaces-api-bind-address` is set. The HTTPRoute for `Exact
/jupyter/api/namespaces` points here, behind the web app's SecurityPolicy, so
all requests carry `--namespaces-api-user-header` (default `kubeflow-userid`,
set by the ext_authz/jwt filters) instead of a client-supplied header.

The handler lists the Active namespaces and returns only those where the user
may `list notebooks.kubeflow.org` (SubjectAccessReview for the user, without
groups — the same check the web app performs when entering a namespace). A
failure of any review answers HTTP 500; answers are cached per user for
`--namespaces-api-cache-ttl` (default `30s`). The response format matches the
upstream backend, e.g.
`{"status":200,"success":true,"user":"...","namespaces":["team-a"]}`.

The server runs on every replica (no leader election) alongside the manager.

### Reconcile triggers

* Notebook create, spec (generation) change or annotation change
* Changes to owned HTTPRoutes/SecurityPolicies/NetworkPolicies (manual edits
  are reverted)
* Role/RoleBinding changes → all notebooks in that namespace
* ClusterRole/ClusterRoleBinding changes → all notebooks
* Periodic resync (`--resync-period`), for RBAC sources that can't be watched,
  such as webhook authorizers

## Layout

```
cmd/                  entrypoint (flags, manager)
internal/config/      configuration flags and validation
internal/access/      candidate subjects + SubjectAccessReview evaluation
internal/routes/      rendering of HTTPRoute, SecurityPolicy, NetworkPolicy, ReferenceGrant
internal/controller/  reconciler and watches
tests/                envtest integration tests (real kube-apiserver with RBAC)
```

## Flags

| Flag | Default | Description |
|---|---|---|
| `--gateway-namespace`, `--gateway-name`, `--gateway-section-name` | `kubeflow`, `kubeflow-gateway`, `""` | parentRef of generated routes |
| `--hostnames` | `""` | Comma-separated route hostnames |
| `--service-port` | `80` | Notebook Service port (upstream `DefaultServingPort`) |
| `--ext-auth-service-name`, `--ext-auth-service-namespace`, `--ext-auth-service-port` | `oauth2-proxy`, `kubeflow`, `4180` | External authorization (ext_authz) service the per-notebook SecurityPolicies point to; its namespace hosts the ReferenceGrants |
| `--ext-auth-headers-to-backend` | `authorization` | Ext-auth response headers forwarded to the notebook (the ID token arrives as `Authorization: Bearer`) |
| `--ext-auth-headers-to-ext-auth` | `cookie, x-forwarded-proto, x-forwarded-host, x-forwarded-for` | Client request headers included in the ext-auth check request; Envoy's HTTP ext_authz default set omits them, so the session cookie must be listed explicitly |
| `--jwt-provider` | placeholder | JSON Envoy Gateway `JWTProvider`, embedded in every per-notebook SecurityPolicy; validates the token headers forwarded by the ext-auth service |
| `--user-claim`, `--groups-claim` | `email`, `groups` | Claims matched against RBAC Users/Groups (`--groups-claim=""` disables groups) |
| `--user-prefix`, `--group-prefix` | `""` | API-server OIDC prefixes of RBAC subjects |
| `--ignored-subject-prefixes` | `system:` | Subjects never exposed as principals |
| `--access-verb` | `get` | Verb on `notebooks.kubeflow.org/<name>` |
| `--remove-request-headers` | `authorization` | Headers stripped before the notebook (the client must not set the header the ext-auth service controls) |
| `--network-policy`, `--network-policy-peers` | `false`, `""` | Per-notebook NetworkPolicy and its allowed peers (JSON `[]NetworkPolicyPeer`) |
| `--resync-period` | `10m` | Periodic re-evaluation |
| `--namespaces-api-bind-address` | `""` | HTTP address serving `GET /jupyter/api/namespaces` for the Jupyter web app (empty disables it) |
| `--namespaces-api-user-header` | `kubeflow-userid` | Header carrying the authenticated user (trusted: set by the web app's ext_authz+jwt SecurityPolicy) |
| `--namespaces-api-cache-ttl` | `30s` | How long a user's answer is cached |
| `--max-concurrent-reconciles` | `4` | Worker count |
| `--leader-elect` | `false` | Leader election (needed for >1 replica) |
| `--metrics-bind-address`, `--health-probe-bind-address` | `:8080`, `:8081` | Endpoints |

## Required permissions

* `kubeflow.org` notebooks: get/list/watch; `notebooks/finalizers`: update
* HTTPRoutes, SecurityPolicies, NetworkPolicies: full; ReferenceGrants:
  get/create/update/patch/delete (in the ext-auth namespace only)
* Namespaces: get/list/watch (for the namespaces API)
* RBAC objects: get/list/watch (read-only)
* `subjectaccessreviews`: create
* `events.k8s.io` events: create/patch

The chart's `templates/route-controller.yaml` contains the exact ClusterRole.

## Build and test

Requires Go 1.25+. Integration tests also need Helm (to extract the pinned
CRDs from the chart dependency) and download envtest binaries into `bin/`.

```sh
make test               # unit tests
make test-integration   # envtest: real kube-apiserver + RBAC authorizer + pinned
                        # Gateway API / Envoy Gateway v1.9.3 / Notebook CRDs;
                        # also server-side dry-runs the rendered Helm chart
make build              # bin/manager
make docker-build IMG=registry.example.com/notebook-route-controller:0.1.0
make docker-buildx IMG=...   # multi-arch push
make helm-lint          # lint + render the chart with every example
make sync-notebook-crd KF_NOTEBOOKS_VERSION=v1.11.1   # refresh the chart's CRD
```

`make run` runs the controller against the current kubeconfig context.
