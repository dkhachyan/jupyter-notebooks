# kubeflow-notebooks-no-istio

Runs Kubeflow Notebooks without Istio. Notebooks are exposed through
[Gateway API](https://gateway-api.sigs.k8s.io/) HTTPRoutes on
[Envoy Gateway](https://gateway.envoyproxy.io/). Login uses your existing OIDC
provider, and access to each Notebook is decided by Kubernetes RBAC.

The chart deploys:

| Component | Source | Changes |
|---|---|---|
| notebook-controller | `ghcr.io/kubeflow/notebooks/notebook-controller:v1.11.1` | none (`USE_ISTIO=false`) |
| Jupyter web app | `ghcr.io/kubeflow/notebooks/jupyter-web-app:v1.11.1` | none |
| oauth2-proxy | `quay.io/oauth2-proxy/oauth2-proxy:v7.15.6` | login service (ext_authz backend) |
| `notebooks.kubeflow.org` CRD | kubeflow/notebooks `v1.11.1`, `config/crd` | none (rendered with kustomize) |
| notebook-route-controller | `controller/` in this repository | new |
| Envoy Gateway (optional dependency) | `oci://docker.io/envoyproxy/gateway-helm:v1.9.3` | none |

**Upstream patches: none.** The upstream notebook-controller already supports
running without Istio (`USE_ISTIO=false`). In that mode it still manages
StatefulSets, Services, PVC handling and culling, and simply skips creating
VirtualServices. Everything Istio used to provide is rebuilt from configuration
plus the new route controller, so this chart has no `upstream-patches/`
directory.

## How it works

```
browser ──► Envoy Gateway
        │
        ├── HTTPRoute <fullname>-oauth2-proxy  /oauth2/*       ──► oauth2-proxy:4180
        │       (login, callback, sign-out; no SecurityPolicy)
        │
        └── every other route created by this chart, each carrying its own
            SecurityPolicy (extAuth + jwt [+ authorization for notebooks]):

            1. ext_authz check ──► oauth2-proxy
               no valid session cookie → oauth2-proxy answers with a 302 to the
               IdP, Envoy Gateway sends it to the browser;
               valid session → oauth2-proxy answers OK and returns
               `Authorization: Bearer <id_token>`, which Envoy Gateway forwards
               to the next steps (headersToBackend overwrites any client header)
            2. JWT filter: validates the ID token against the IdP's JWKS
               (issuer, audiences) and sets kubeflow-userid from the user claim
               (claimToHeaders overwrites any client-supplied value)
            3. Authorization (notebook routes only): allow if the user claim /
               groups claim is in the notebook's RBAC allow-list
            ▼
            HTTPRoute backend (web app or Notebook server)
```

Login lives in **oauth2-proxy**, a small deployment of this chart in the
release namespace. `/oauth2/*` is routed to it by an HTTPRoute without any
SecurityPolicy — the ext_authz check of every other route redirects browsers
there when there is no valid session.

By default the session (the ID token OAuth2-proxy received) is stored in a
browser cookie. With IdPs that include every group in ID tokens, the token —
and with it the cookie — can grow past the header size limits of browsers and
Envoy Gateway (60 KB). `oauth2Proxy.sessionStore.type=redis` moves the session
into a single-replica Redis (deployed by the chart in the release namespace
without persistence, or an external one); only an opaque session cookie worn
by the browser remains.

For each Notebook, the **notebook-route-controller** creates the following
objects in the Notebook's namespace. Each is owned by the Notebook and is
garbage-collected with it:

* **HTTPRoute** `notebook-<ns>-<name>`. Same path, backend, rewrite and header
  behaviour as the upstream Istio VirtualService. It honours the
  `notebooks.kubeflow.org/http-rewrite-uri` and
  `notebooks.kubeflow.org/http-headers-request-set` annotations.
* **SecurityPolicy** `notebook-<ns>-<name>` with `extAuth` (oauth2-proxy as the
  ext_authz backend), the JWT provider and an `authorization` with
  `defaultAction: Deny` that allows only the users and groups described in
  [Authorization](#authorization). Because the ext_authz backend lives in
  another namespace, the controller also creates a **ReferenceGrant** in the
  release namespace allowing these SecurityPolicies to reference the
  oauth2-proxy Service.
* **NetworkPolicy** `notebook-<ns>-<name>` (when `networkPolicy.enabled`). Only
  the gateway proxies and the notebook-controller can reach the notebook server
  port. All other ports stay open.

The chart itself creates: the oauth2-proxy deployment, Service, cookie Secret
and its own `/oauth2` HTTPRoute, a SecurityPolicy with the same ext_authz + JWT
setup for the Jupyter web app route (`/jupyter`, with `/` redirecting to it),
an optional Gateway, and nothing that targets the Gateway itself — the chart
works on a shared listener used by other teams as well.

### Gateway objects

| Created by | Object | API group / version | Namespace |
|---|---|---|---|
| Chart | HTTPRoute `<fullname>-jupyter-web-app` (`/jupyter`, `/` redirect, `/api/namespaces`) | `gateway.networking.k8s.io/v1` | release |
| Chart | HTTPRoute `<fullname>-oauth2-proxy` (`/oauth2/*`, no policy) | `gateway.networking.k8s.io/v1` | release |
| Chart | SecurityPolicy `<fullname>-jupyter-web-app` (extAuth + JWT) | `gateway.envoyproxy.io/v1alpha1` | release |
| Chart (`gateway.create=true` only) | Gateway `gateway.name` | `gateway.networking.k8s.io/v1` | `gateway.namespace` |
| Controller | HTTPRoute `notebook-<ns>-<name>`, one per Notebook | `gateway.networking.k8s.io/v1` | notebook |
| Controller | SecurityPolicy `notebook-<ns>-<name>`, one per Notebook (extAuth + JWT + authorization) | `gateway.envoyproxy.io/v1alpha1` | notebook |
| Controller | ReferenceGrant `notebook-extauth-<ns>`, one per notebook namespace other than the release namespace | `gateway.networking.k8s.io/v1beta1` | release |
| Controller | NetworkPolicy `notebook-<ns>-<name>`, one per Notebook (Kubernetes, not Envoy Gateway) | `networking.k8s.io/v1` | notebook |

Every SecurityPolicy targets an HTTPRoute (`targetRefs[].kind: HTTPRoute`),
never a Gateway or listener. The chart and the controller create no
ClientTrafficPolicy, BackendTrafficPolicy, EnvoyExtensionPolicy,
EnvoyPatchPolicy, HTTPRouteFilter, Backend or EnvoyProxy, and nothing in the
Gateway's namespace unless `gateway.create=true`.

### Authorization

Kubernetes decides who may open a notebook. No separate authorization service
is involved:

1. The controller collects candidate subjects: `User` and `Group` subjects of
   RoleBindings in the notebook namespace and of all ClusterRoleBindings. It
   skips names starting with `routeController.ignoredSubjectPrefixes`
   (`system:`) and names without `userPrefix`/`groupPrefix`.
2. For every candidate it creates a **SubjectAccessReview** for
   `<accessVerb> notebooks.kubeflow.org/<name>` in the namespace (default
   `get`). The API server answers it with RBAC or any other configured
   authorizer, so `resourceNames`, aggregated roles and webhooks all apply.
3. Allowed subjects are written into the notebook's SecurityPolicy, matched
   against `auth.jwt.userClaim` (users) and `auth.jwt.groupsClaim` with
   `StringArray` (groups).

The access list is recomputed when the Notebook changes or when any Role,
RoleBinding, ClusterRole or ClusterRoleBinding changes. It is also recomputed
every `routeController.resyncPeriod` (default 10m). When nobody is allowed, the
policy denies every request and the Notebook gets a `NoAccess` warning event.
If an access review fails, the previous policy stays in place and the
controller retries. It never falls back to "authenticated users".

With Kubeflow Profiles this behaves like the Istio setup. The profile owner and
contributors have RoleBindings to `kubeflow-admin`/`kubeflow-edit`/`kubeflow-view`,
which aggregate `kubeflow-notebooks-*` (installed by this chart). Because of that
they can `get` notebooks in the namespace and are allowed through the gateway.

The Jupyter web app keeps its own upstream authorization. It reads the user
from `kubeflow-userid`, which only the gateway can set, and checks every API
call with SubjectAccessReviews.

#### Namespace list in the web app

The upstream web app fills its namespace dropdown from
`GET <prefix>/api/namespaces`; without a central dashboard that endpoint shows
either all namespaces of the cluster or none, so every user would need
cluster-wide `list namespaces`. With `jupyterWebApp.namespaceFilter.enabled`
(default) that request is served by a small HTTP endpoint of the
notebook-route-controller instead: the first rule of the web app HTTPRoute
sends `<prefix>/api/namespaces` to it, past the same SecurityPolicy, so the
request carries a gateway-set trustable `kubeflow-userid`. The endpoint returns
only the namespaces where that user may list notebooks. Like the web app
itself, it authorizes on the user only — group memberships are not evaluated.
Set it to `false` for the upstream behaviour.

#### Identity mapping requirements

* The user claim value (default `email`) must equal the RBAC `User` subject
  name after `routeController.userPrefix` is stripped. Groups work the same way.
  This matches how the API server maps OIDC tokens when it is configured against
  the same issuer.
* Users who get access only through a group are allowed only if the IdP puts
  that group into the `groups` claim. Group memberships that exist only inside
  Kubernetes (impersonation, `system:` groups) are not visible to the gateway.

## Prerequisites

* Kubernetes ≥ 1.29.
* **Envoy Gateway ≥ v1.6** with Gateway API CRDs (per-route `SecurityPolicy`
  with `extAuth.http` and `headersToBackend`). No gateway-level policies are
  used, so `SecurityPolicy.mergeType` is not needed. You can install Envoy
  Gateway separately (recommended) or set `envoy-gateway.enabled=true`.
* An **OIDC provider** (Dex, Keycloak, Entra, ...) that issues ID tokens with
  the user claim (and optionally `groups`) on every token. Register the
  redirect URI `https://<gateway.hostname>/oauth2/callback`.
* A Secret with key `client-secret` in the **release namespace**, referenced by
  `auth.oidc.clientSecret.existingSecret`.
* A TLS Secret for the listener (`gateway.listener.tlsSecretName`) when
  `gateway.create=true`.
* A **CNI that enforces NetworkPolicy** with `endPort` support (Calico, Cilium,
  ...) when `networkPolicy.enabled=true` (default). Without it, notebooks are
  reachable inside the cluster without passing the gateway.
* Namespaces and RBAC for users, for example from the Kubeflow Profile
  controller or plain RoleBindings.
* A **shared Gateway listener is fine**: the chart only attaches HTTPRoutes and
  route-level SecurityPolicies, so it does not affect other routes on the same
  listener and creates nothing in the Gateway's namespace.

## Install

```sh
helm dependency build charts/kubeflow-notebooks-no-istio

kubectl create namespace kubeflow
kubectl -n kubeflow create secret generic kubeflow-notebooks-oidc \
  --from-literal=client-secret='<secret>'

helm upgrade --install kubeflow-notebooks charts/kubeflow-notebooks-no-istio \
  -n kubeflow -f charts/kubeflow-notebooks-no-istio/examples/dex.yaml
```

Build and push the route controller image first (`make -C controller
docker-buildx IMG=...`), then set `routeController.image`.

Example values:

| File | Scenario |
|---|---|
| `examples/dex.yaml` | Existing Dex and Envoy Gateway; the chart creates the Gateway |
| `examples/keycloak-existing-gateway.yaml` | Keycloak, a Gateway managed elsewhere, API server OIDC prefixes, HA, culling |
| `examples/dev-kind.yaml` | kind/dev: Envoy Gateway as a dependency, inline client secret |
| `examples/shared-gateway.yaml` | A shared listener owned by another team, in-cluster JWKS URI, IdP behind a private CA |

The CRD in `crds/` is installed on first install only (Helm behaviour). Apply
it manually on upgrades:
`kubectl apply --server-side -f charts/kubeflow-notebooks-no-istio/crds/`.

## Key values

| Value | Default | Description |
|---|---|---|
| `gateway.hostname` | — (required) | Public hostname; set on the listener and all routes |
| `gateway.create` / `gateway.name` / `gateway.namespace` / `gateway.sectionName` | `true` / `kubeflow-gateway` / release ns / `https` | Gateway and listener the routes attach to |
| `auth.oidc.issuer`, `clientID`, `jwksURI` | — (required) | Existing OIDC provider; `jwksURI` may be an in-cluster `http://` URL |
| `auth.oidc.clientSecret.existingSecret` | — | Secret (key `client-secret`) in the release namespace |
| `auth.jwt.userClaim` / `groupsClaim` | `email` / `groups` | Claims matched against RBAC subjects |
| `auth.userIdHeader` | `kubeflow-userid` | Identity header for Kubeflow web apps (set from the validated ID token) |
| `oauth2Proxy.cookieSecret.existingSecret` / `.value` | — / — | Session cookie secret in the release namespace (value must be 16, 24 or 32 bytes) |
| `oauth2Proxy.cookieName` / `cookieExpire` / `cookieDomain` | `_oauth2_proxy_kubeflow` / `24h` / `""` | Session cookie settings |
| `oauth2Proxy.sessionStore.type` / `sessionStore.redis.*` | `cookie` / — | Session storage; use `redis` when ID tokens are large |
| `oauth2Proxy.apiRoutes` | jupyter + notebook API paths | Paths that get `401` instead of a login redirect |
| `oauth2Proxy.headersToExtAuth` | `cookie`, `x-forwarded-*` | Headers forwarded from Envoy Gateway to oauth2-proxy; must contain `cookie` |
| `routeController.accessVerb` | `get` | Verb checked on `notebooks/<name>` |
| `routeController.userPrefix` / `groupPrefix` | `""` | API server `--oidc-username-prefix` / `--oidc-groups-prefix` |
| `networkPolicy.enabled` / `gatewayPeers` | `true` / Envoy pods in `envoy-gateway-system` | Per-notebook NetworkPolicy |
| `notebookController.culling.*` | disabled | Upstream idle culling |
| `jupyterWebApp.enabled` / `prefix` / `spawnerUIConfig` | `true` / `/jupyter` / image default | Upstream notebook UI |
| `jupyterWebApp.namespaceFilter.enabled` | `true` | Namespace dropdown shows only namespaces where the user may list notebooks (needs the route controller endpoint) |
| `envoy-gateway.enabled` | `false` | Install Envoy Gateway as a dependency |

See `values.yaml` for every option; `values.schema.json` validates them.

## Security notes

* **Header spoofing.** A client cannot forge `kubeflow-userid` or
  `Authorization`: on every protected route the SecurityPolicy makes Envoy
  Gateway overwrite both — ext_authz `headersToBackend` replaces
  `Authorization` with the ID token returned by oauth2-proxy, and the JWT
  filter's `claimToHeaders` overwrites `kubeflow-userid` with the claim from
  that validated token. This only holds because JWT validation runs on every
  route created by this chart; a route to the same hostname without such a
  policy (created by something else) would trust whatever headers the client
  sends.
* **Missing claim.** If the ID token does not contain the user claim, the JWT
  filter does not set `kubeflow-userid` and the web app / notebook
  authorization fails closed. Configure your IdP so that the claim is always
  present.
* **Token exposure.** The ID token travels to the backend in `Authorization`
  (ext_authz `headersToBackend`) and is stripped on notebook routes
  (`--remove-request-headers`). Like the Istio + oauth2-proxy setup, the
  gateway session cookies are still sent to the notebook server. A notebook
  owner can read the cookies of users who open their notebook. Only RBAC
  subjects of that notebook can open it.
* **No cookie refresh.** `--cookie-refresh=0` on purpose: Envoy Gateway does
  not forward the ext_authz response headers to the browser on a successful
  check, so a refreshed cookie set by oauth2-proxy would silently never reach
  the client. Keep `cookieExpire` at or below your IdP's ID token lifetime; an
  expired session means a new redirect to the IdP.
* **Other routes on the same hostname.** Routes not created by this chart or
  the notebook-route-controller carry no policy and are reachable without
  login. Every route that must be protected needs its own SecurityPolicy.
* **Network bypass.** Keep `networkPolicy.enabled=true` and make sure
  `gatewayPeers` matches your Envoy proxy pods. With Envoy Gateway's
  `GatewayNamespace` deployment mode or the bundled dependency, the proxies run
  in the Gateway's namespace (see the examples).

## Migrating from the Istio setup

1. Install this chart next to your existing setup, with a different hostname or
   listener.
2. Switch the existing notebook-controller to `USE_ISTIO=false` (or uninstall it
   and let this chart's controller take over; both run the same image). The
   upstream controller does not delete VirtualServices it created earlier.
   After cutover, delete the VirtualServices named `notebook-<ns>-<name>`
   yourself.
3. Notebook URLs stay the same: `/notebook/<ns>/<name>/`.

## Troubleshooting

```sh
# Generated objects and their status (Accepted / ResolvedRefs from Envoy Gateway)
kubectl -n <ns> get httproute,securitypolicy,networkpolicy -l app.kubernetes.io/managed-by=notebook-route-controller
kubectl -n <ns> describe securitypolicy notebook-<ns>-<name>
# Controller decisions (NoAccess, AccessReviewFailed, InvalidAnnotation, ApplyFailed)
kubectl -n <ns> get events --field-selector involvedObject.kind=Notebook
# Check what RBAC says for a user
kubectl auth can-i get notebooks.kubeflow.org/<name> -n <ns> --as alice@example.com
```

A `403 RBAC: access denied` from Envoy means the user's claim is not in the
notebook's allow-list. Check the claim values in the ID token against the RBAC
subject names and prefixes.
