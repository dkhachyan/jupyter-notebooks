// Package routes renders the HTTPRoute and SecurityPolicy that expose a
// Notebook through a Gateway API gateway (Envoy Gateway).
//
// The routing mirrors the Istio VirtualService that the upstream
// notebook-controller creates when USE_ISTIO=true: requests under
// /notebook/<namespace>/<name>/ are sent to the notebook Service on port 80,
// honouring the notebooks.kubeflow.org/http-rewrite-uri and
// notebooks.kubeflow.org/http-headers-request-set annotations.
package routes

import (
	"encoding/json"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"notebook-route-controller/internal/access"
	"notebook-route-controller/internal/config"
)

// Annotations understood by the upstream notebook-controller.
const (
	AnnotationRewriteURI        = "notebooks.kubeflow.org/http-rewrite-uri"
	AnnotationHeadersRequestSet = "notebooks.kubeflow.org/http-headers-request-set"
)

// Labels set on generated objects.
const (
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	LabelNotebookName = "notebooks.kubeflow.org/notebook-name"
	ManagerName       = "notebook-route-controller"
)

var (
	NotebookGVK       = schema.GroupVersionKind{Group: "kubeflow.org", Version: "v1", Kind: "Notebook"}
	HTTPRouteGVK      = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	SecurityPolicyGVK = schema.GroupVersionKind{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "SecurityPolicy"}
	NetworkPolicyGVK  = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
)

// Name returns the name of the generated objects, matching the name of the
// VirtualService the upstream controller would create.
func Name(namespace, notebook string) string {
	return fmt.Sprintf("notebook-%s-%s", namespace, notebook)
}

// PathPrefix returns the URL prefix of a notebook (the upstream NB_PREFIX).
func PathPrefix(namespace, notebook string) string {
	return fmt.Sprintf("/notebook/%s/%s", namespace, notebook)
}

// HTTPRoute renders the route for a notebook. Warnings describe annotation
// values that were ignored.
func HTTPRoute(namespace, notebook string, annotations map[string]string, cfg config.Config) (*unstructured.Unstructured, []string) {
	var warnings []string

	parentRef := map[string]any{
		"group":     "gateway.networking.k8s.io",
		"kind":      "Gateway",
		"namespace": cfg.GatewayNamespace,
		"name":      cfg.GatewayName,
	}
	if cfg.GatewaySectionName != "" {
		parentRef["sectionName"] = cfg.GatewaySectionName
	}

	var filters []any
	if rewrite := annotations[AnnotationRewriteURI]; rewrite != "" {
		filters = append(filters, map[string]any{
			"type": "URLRewrite",
			"urlRewrite": map[string]any{
				"path": map[string]any{"type": "ReplacePrefixMatch", "replacePrefixMatch": rewrite},
			},
		})
	}

	headerMod := map[string]any{}
	if raw := annotations[AnnotationHeadersRequestSet]; raw != "" {
		set := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &set); err != nil {
			warnings = append(warnings, fmt.Sprintf("ignoring %s: %v", AnnotationHeadersRequestSet, err))
		}
		names := make([]string, 0, len(set))
		for k := range set {
			if config.ValidHeaderName(k) {
				names = append(names, k)
			} else {
				warnings = append(warnings, fmt.Sprintf("ignoring invalid header name %q in %s", k, AnnotationHeadersRequestSet))
			}
		}
		sort.Strings(names)
		var headers []any
		for _, k := range names {
			headers = append(headers, map[string]any{"name": k, "value": set[k]})
		}
		if len(headers) > 0 {
			headerMod["set"] = headers
		}
	}
	if len(cfg.RemoveRequestHeaders) > 0 {
		remove := make([]any, 0, len(cfg.RemoveRequestHeaders))
		for _, h := range cfg.RemoveRequestHeaders {
			remove = append(remove, h)
		}
		headerMod["remove"] = remove
	}
	if len(headerMod) > 0 {
		filters = append(filters, map[string]any{"type": "RequestHeaderModifier", "requestHeaderModifier": headerMod})
	}

	rule := map[string]any{
		"matches": []any{map[string]any{
			"path": map[string]any{"type": "PathPrefix", "value": PathPrefix(namespace, notebook)},
		}},
		"backendRefs": []any{map[string]any{
			"name": notebook,
			"port": int64(cfg.ServicePort),
		}},
	}
	if len(filters) > 0 {
		rule["filters"] = filters
	}

	spec := map[string]any{
		"parentRefs": []any{parentRef},
		"rules":      []any{rule},
	}
	if len(cfg.Hostnames) > 0 {
		hosts := make([]any, 0, len(cfg.Hostnames))
		for _, h := range cfg.Hostnames {
			hosts = append(hosts, h)
		}
		spec["hostnames"] = hosts
	}

	u := newObject(HTTPRouteGVK, namespace, notebook)
	u.Object["spec"] = spec
	return u, warnings
}

// SecurityPolicy renders the self-contained per-notebook policy. The extAuth
// filter calls the external authorization service (oauth2-proxy), which
// returns an Authorization: Bearer header on success; the jwt filter then
// validates that token and authorization denies requests unless the validated
// token carries one of the allowed users or groups. An empty allow-list
// denies everyone.
func SecurityPolicy(namespace, notebook string, p access.Principals, cfg config.Config) (*unstructured.Unstructured, error) {
	provider, err := cfg.Provider()
	if err != nil {
		return nil, err
	}
	name := provider["name"].(string)

	var rules []any
	if len(p.Users) > 0 {
		rules = append(rules, jwtRule("allow-users", name, cfg.UserClaim, "", p.Users))
	}
	if len(p.Groups) > 0 && cfg.GroupsClaim != "" {
		rules = append(rules, jwtRule("allow-groups", name, cfg.GroupsClaim, "StringArray", p.Groups))
	}

	authz := map[string]any{"defaultAction": "Deny"}
	if len(rules) > 0 {
		authz["rules"] = rules
	}

	headers := make([]any, 0, len(cfg.ExtAuthHeadersToBackend))
	for _, h := range cfg.ExtAuthHeadersToBackend {
		headers = append(headers, h)
	}
	toExtAuth := make([]any, 0, len(cfg.ExtAuthHeadersToExtAuth))
	for _, h := range cfg.ExtAuthHeadersToExtAuth {
		toExtAuth = append(toExtAuth, h)
	}

	u := newObject(SecurityPolicyGVK, namespace, notebook)
	u.Object["spec"] = map[string]any{
		"targetRefs": []any{map[string]any{
			"group": HTTPRouteGVK.Group,
			"kind":  HTTPRouteGVK.Kind,
			"name":  Name(namespace, notebook),
		}},
		"extAuth": map[string]any{
			// Without the session cookie the ext-auth service would never see
			// an established session (the Envoy HTTP ext_authz default set does
			// not include it), so the login would loop.
			"headersToExtAuth": toExtAuth,
			"http": map[string]any{
				"backendRefs": []any{map[string]any{
					"name":      cfg.ExtAuthServiceName,
					"namespace": cfg.ExtAuthServiceNamespace,
					"port":      int64(cfg.ExtAuthServicePort),
				}},
				"headersToBackend": headers,
			},
		},
		"authorization": authz,
		// The policy must define its own provider: the jwt filter validates the
		// token the ext-auth service forwarded (Authorization: Bearer).
		"jwt": map[string]any{"providers": []any{provider}},
	}
	return u, nil
}

func jwtRule(name, provider, claim, valueType string, values []string) map[string]any {
	vals := make([]any, 0, len(values))
	for _, v := range values {
		vals = append(vals, v)
	}
	c := map[string]any{"name": claim, "values": vals}
	if valueType != "" {
		c["valueType"] = valueType
	}
	return map[string]any{
		"name":   name,
		"action": "Allow",
		"principal": map[string]any{
			"jwt": map[string]any{"provider": provider, "claims": []any{c}},
		},
	}
}

func newObject(gvk schema.GroupVersionKind, namespace, notebook string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(Name(namespace, notebook))
	u.SetLabels(map[string]string{
		LabelManagedBy:    ManagerName,
		LabelNotebookName: notebook,
	})
	return u
}
