# jupyter-notebooks

Kubeflow Notebooks without Istio. The upstream notebook-controller and Jupyter
web app run unmodified; notebooks are exposed through
[Gateway API](https://gateway-api.sigs.k8s.io/) HTTPRoutes on
[Envoy Gateway](https://gateway.envoyproxy.io/), users log in with an existing
OIDC provider through oauth2-proxy, and access to every notebook is decided by
Kubernetes RBAC.

- **No Istio, no upstream patches.** `USE_ISTIO=false` in the upstream
  notebook-controller; routing and authorization that Istio used to provide are
  rebuilt with Gateway API, Envoy Gateway `SecurityPolicy` and a small
  controller from this repository.
- **Per-notebook access from RBAC.** Whoever may `get` a Notebook (users and
  groups, via RoleBindings, ClusterRoleBindings, aggregated roles or webhook
  authorizers) can open it through the gateway; nobody else can.
- **Works on a shared Gateway.** Only HTTPRoutes and route-level policies are
  created; nothing targets the Gateway or listener, so other teams' routes on the
  same listener are not affected.

## Repository layout

| Path | Contents |
|---|---|
| [`charts/kubeflow-notebooks-no-istio`](charts/kubeflow-notebooks-no-istio/README.md) | Helm chart: notebook-controller, Jupyter web app, oauth2-proxy (+ optional Redis), notebook-route-controller, Notebook CRD. Architecture, values, security notes and troubleshooting are documented there. |
| [`controller`](controller/README.md) | `notebook-route-controller` (Go): per-Notebook HTTPRoute, SecurityPolicy, NetworkPolicy and ReferenceGrant, plus the namespace list endpoint for the web app. |
| `.github/workflows` | CI (unit + envtest integration tests, chart lint) and the release workflow. |

## How it works

```
browser ─► Envoy Gateway ─┬─ /oauth2/*        ─► oauth2-proxy (login, no policy)
                          ├─ /jupyter, /      ─► Jupyter web app
                          └─ /notebook/<ns>/<name>/ ─► notebook server
                               every route except /oauth2 has its own SecurityPolicy:
                               ext_authz (oauth2-proxy session) → JWT check → RBAC allow-list
```

Details: [How it works](charts/kubeflow-notebooks-no-istio/README.md#how-it-works),
[Authorization](charts/kubeflow-notebooks-no-istio/README.md#authorization),
[Gateway objects](charts/kubeflow-notebooks-no-istio/README.md#gateway-objects).

## Install

Prerequisites (full list in the
[chart README](charts/kubeflow-notebooks-no-istio/README.md#prerequisites)):
Kubernetes ≥ 1.29, Envoy Gateway ≥ v1.6 with a Gateway/listener for the
hostname, an OIDC provider with the redirect URI
`https://<hostname>/oauth2/callback`, and a Secret with its client secret.

The chart and the controller image are published to GitHub Container Registry:

```sh
kubectl create namespace kubeflow
kubectl -n kubeflow create secret generic kubeflow-notebooks-oidc \
  --from-literal=client-secret='<secret>'

helm upgrade --install kubeflow-notebooks \
  oci://ghcr.io/dkhachyan/charts/kubeflow-notebooks-no-istio --version 0.2.0 \
  -n kubeflow -f my-values.yaml
```

Start `my-values.yaml` from one of the
[examples](charts/kubeflow-notebooks-no-istio/examples): `dex.yaml` (the chart
creates the Gateway), `shared-gateway.yaml` (attach to an existing listener),
`keycloak-existing-gateway.yaml`, `dev-kind.yaml` (local kind cluster).

### Namespaces for users

The chart does not create user namespaces. For every namespace that should hold
notebooks:

```sh
kubectl create namespace team-a
# The web app runs notebook pods as "default-editor" (the Kubeflow Profile
# controller would create it). It needs no permissions unless notebook code
# should call the Kubernetes API.
kubectl -n team-a create serviceaccount default-editor
# Everything the web app checks to list, create and open notebooks.
kubectl -n team-a create rolebinding alice \
  --clusterrole=kubeflow-notebooks-user --user=alice@example.com
```

The RBAC user name must equal the value of `auth.jwt.userClaim` in the ID token
(after `routeController.userPrefix` is stripped).

## Known limitations

- The upstream web app authorizes API calls for the **user only**: rights that
  come only from groups work at the gateway but not in the web app UI.
- Without the Kubeflow central dashboard the web app first opens the
  `kubeflow-user` namespace, so a 403 for it can appear once; pick a namespace in
  the selector.
- Notebook ports are protected in-cluster only by NetworkPolicy. Without a CNI
  that enforces it, pods can reach notebook servers directly, bypassing the
  gateway.
- Namespaces with Istio sidecar injection (for example Kubeflow profiles of an
  existing Istio-based install) reject traffic from Envoy Gateway; notebooks
  there have to be created without the sidecar.

## Development

```sh
make -C controller test               # unit tests
make -C controller test-integration   # envtest: real kube-apiserver, RBAC and the pinned CRDs
make -C controller helm-lint          # lint and render the chart with every example
make -C controller docker-buildx IMG=<registry>/notebook-route-controller:<tag>
```

`make -C controller sync-notebook-crd` re-renders the upstream Notebook CRD into
the chart.

## Releases

Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yaml`: it builds
`ghcr.io/<owner>/notebook-route-controller:X.Y.Z` (amd64, arm64) and pushes the
chart `oci://ghcr.io/<owner>/charts/kubeflow-notebooks-no-istio` with version
and `appVersion` `X.Y.Z`. Both GHCR packages must grant this repository write
access (*Package settings → Manage Actions access*) and be public for anonymous
pulls.
