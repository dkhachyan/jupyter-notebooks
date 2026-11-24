package routes

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"notebook-route-controller/internal/access"
	"notebook-route-controller/internal/config"
)

func nested(t *testing.T, obj map[string]any, path ...any) any {
	t.Helper()
	var cur any = obj
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("path %v: %T is not a map", path, cur)
			}
			cur = m[k]
		case int:
			s, ok := cur.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("path %v: %T has no index %d", path, cur, k)
			}
			cur = s[k]
		}
	}
	return cur
}

func TestHTTPRouteDefaults(t *testing.T) {
	cfg := config.Default()
	route, warnings := HTTPRoute("alice", "lab", nil, cfg)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if route.GetName() != "notebook-alice-lab" || route.GetNamespace() != "alice" {
		t.Fatalf("unexpected identity %s/%s", route.GetNamespace(), route.GetName())
	}
	if got := route.GetLabels()[LabelManagedBy]; got != ManagerName {
		t.Fatalf("managed-by label = %q", got)
	}
	spec := route.Object["spec"].(map[string]any)

	wantParent := map[string]any{
		"group": "gateway.networking.k8s.io", "kind": "Gateway",
		"namespace": "kubeflow", "name": "kubeflow-gateway",
	}
	if got := nested(t, spec, "parentRefs", 0); !reflect.DeepEqual(got, wantParent) {
		t.Fatalf("parentRef = %v", got)
	}
	if _, ok := spec["hostnames"]; ok {
		t.Fatal("hostnames must be omitted when not configured")
	}
	if got := nested(t, spec, "rules", 0, "matches", 0, "path", "value"); got != "/notebook/alice/lab" {
		t.Fatalf("path prefix = %v", got)
	}
	backend := nested(t, spec, "rules", 0, "backendRefs", 0).(map[string]any)
	if backend["name"] != "lab" || backend["port"] != int64(80) {
		t.Fatalf("backend = %v", backend)
	}
	// The client-fed Authorization header is always stripped before the ext-auth
	// service replaces it with the session token.
	filters := nested(t, spec, "rules", 0, "filters").([]any)
	if len(filters) != 1 {
		t.Fatalf("filters = %v", filters)
	}
	if got := nested(t, spec, "rules", 0, "filters", 0, "requestHeaderModifier", "remove"); !reflect.DeepEqual(got, []any{"authorization"}) {
		t.Fatalf("remove headers = %v", got)
	}
	assertSerialisable(t, route)
}

func TestHTTPRouteAnnotationsAndListener(t *testing.T) {
	cfg := config.Default()
	cfg.GatewaySectionName = "https"
	cfg.Hostnames = []string{"kubeflow.example.com"}
	cfg.RemoveRequestHeaders = nil

	route, warnings := HTTPRoute("alice", "rstudio", map[string]string{
		AnnotationRewriteURI:        "/",
		AnnotationHeadersRequestSet: `{"X-RStudio-Root-Path":"/notebook/alice/rstudio/","bad header":"x"}`,
	}, cfg)
	if len(warnings) != 1 {
		t.Fatalf("expected one warning for the invalid header name, got %v", warnings)
	}
	spec := route.Object["spec"].(map[string]any)
	if got := nested(t, spec, "parentRefs", 0, "sectionName"); got != "https" {
		t.Fatalf("sectionName = %v", got)
	}
	if got := spec["hostnames"]; !reflect.DeepEqual(got, []any{"kubeflow.example.com"}) {
		t.Fatalf("hostnames = %v", got)
	}
	rewrite := nested(t, spec, "rules", 0, "filters", 0).(map[string]any)
	if rewrite["type"] != "URLRewrite" || nested(t, rewrite, "urlRewrite", "path", "replacePrefixMatch") != "/" {
		t.Fatalf("rewrite filter = %v", rewrite)
	}
	set := nested(t, spec, "rules", 0, "filters", 1, "requestHeaderModifier", "set")
	want := []any{map[string]any{"name": "X-RStudio-Root-Path", "value": "/notebook/alice/rstudio/"}}
	if !reflect.DeepEqual(set, want) {
		t.Fatalf("header set = %v", set)
	}
	if _, ok := nested(t, spec, "rules", 0, "filters", 1, "requestHeaderModifier").(map[string]any)["remove"]; ok {
		t.Fatal("remove must be omitted when no headers are configured")
	}
	assertSerialisable(t, route)
}

func TestHTTPRouteInvalidHeaderJSON(t *testing.T) {
	cfg := config.Default()
	cfg.RemoveRequestHeaders = nil
	route, warnings := HTTPRoute("ns", "nb", map[string]string{AnnotationHeadersRequestSet: "{not json"}, cfg)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v", warnings)
	}
	if _, ok := nested(t, route.Object, "spec", "rules", 0).(map[string]any)["filters"]; ok {
		t.Fatal("no filters expected for invalid annotation")
	}
}

func TestSecurityPolicy(t *testing.T) {
	cfg := config.Default()
	p, err := SecurityPolicy("alice", "lab", access.Principals{
		Users:  []string{"alice@example.com", "bob@example.com"},
		Groups: []string{"ml-team"},
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.GetName() != "notebook-alice-lab" {
		t.Fatalf("name = %s", p.GetName())
	}
	spec := p.Object["spec"].(map[string]any)
	if _, ok := spec["mergeType"]; ok {
		t.Fatalf("mergeType must be absent, got %v", spec["mergeType"])
	}
	wantTarget := map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "notebook-alice-lab"}
	if got := nested(t, spec, "targetRefs", 0); !reflect.DeepEqual(got, wantTarget) {
		t.Fatalf("targetRef = %v", got)
	}
	wantBackend := map[string]any{"name": "oauth2-proxy", "namespace": "kubeflow", "port": int64(4180)}
	if got := nested(t, spec, "extAuth", "http", "backendRefs", 0); !reflect.DeepEqual(got, wantBackend) {
		t.Fatalf("extAuth backendRef = %v", got)
	}
	if got := nested(t, spec, "extAuth", "http", "headersToBackend"); !reflect.DeepEqual(got, []any{"authorization"}) {
		t.Fatalf("headersToBackend = %v", got)
	}
	if got := nested(t, spec, "extAuth", "headersToExtAuth"); !reflect.DeepEqual(got, []any{"cookie", "x-forwarded-proto", "x-forwarded-host", "x-forwarded-for"}) {
		t.Fatalf("headersToExtAuth = %v", got)
	}
	if got := nested(t, spec, "authorization", "defaultAction"); got != "Deny" {
		t.Fatalf("defaultAction = %v", got)
	}
	users := nested(t, spec, "authorization", "rules", 0, "principal", "jwt").(map[string]any)
	if users["provider"] != "kubeflow" {
		t.Fatalf("provider = %v", users["provider"])
	}
	wantUsers := map[string]any{"name": "email", "values": []any{"alice@example.com", "bob@example.com"}}
	if got := nested(t, users, "claims", 0); !reflect.DeepEqual(got, wantUsers) {
		t.Fatalf("user claim = %v", got)
	}
	wantProvider := map[string]any{"name": "kubeflow", "remoteJWKS": map[string]any{"uri": "https://issuer.invalid/keys"}}
	if got := nested(t, spec, "jwt", "providers", 0); !reflect.DeepEqual(got, wantProvider) {
		t.Fatalf("jwt provider = %v", got)
	}
	wantGroups := map[string]any{"name": "groups", "valueType": "StringArray", "values": []any{"ml-team"}}
	if got := nested(t, spec, "authorization", "rules", 1, "principal", "jwt", "claims", 0); !reflect.DeepEqual(got, wantGroups) {
		t.Fatalf("group claim = %v", got)
	}
	assertSerialisable(t, p)
}

func TestSecurityPolicyDeniesEveryoneWithoutPrincipals(t *testing.T) {
	p, err := SecurityPolicy("ns", "nb", access.Principals{}, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	authz := nested(t, p.Object, "spec", "authorization").(map[string]any)
	if authz["defaultAction"] != "Deny" {
		t.Fatalf("defaultAction = %v", authz["defaultAction"])
	}
	if _, ok := authz["rules"]; ok {
		t.Fatalf("no rules expected, got %v", authz["rules"])
	}
}

func TestSecurityPolicyInvalidProvider(t *testing.T) {
	cfg := config.Default()
	cfg.JWTProvider = "{"
	if _, err := SecurityPolicy("ns", "nb", access.Principals{}, cfg); err == nil {
		t.Fatal("expected error")
	}
}

func TestSecurityPolicyGroupsDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.GroupsClaim = ""
	p, err := SecurityPolicy("ns", "nb", access.Principals{Groups: []string{"g"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := nested(t, p.Object, "spec", "authorization").(map[string]any)["rules"]; ok {
		t.Fatal("group rules must not be rendered when the groups claim is disabled")
	}
}

// assertSerialisable fails if the object contains values the unstructured
// JSON conversion cannot handle (e.g. plain int or []string).
func assertSerialisable(t *testing.T, u *unstructured.Unstructured) {
	t.Helper()
	if _, err := u.MarshalJSON(); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := unstructured.SetNestedField(u.DeepCopy().Object, "x", "probe"); err != nil {
		t.Fatalf("deep copy: %v", err)
	}
}
