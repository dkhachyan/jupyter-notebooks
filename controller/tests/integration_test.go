// Package tests runs the controller against a real kube-apiserver (envtest)
// with the RBAC authorizer enabled, so SubjectAccessReviews are evaluated by
// Kubernetes exactly as in a cluster.
//
// Run with `make test-integration`, which downloads the envtest binaries and
// sets KUBEBUILDER_ASSETS. The tests are skipped when it is not set.
package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"notebook-route-controller/internal/access"
	"notebook-route-controller/internal/config"
	"notebook-route-controller/internal/controller"
	"notebook-route-controller/internal/nsapi"
	"notebook-route-controller/internal/routes"
)

const (
	namespace     = "alice"
	authNamespace = "auth"
	timeout       = 20 * time.Second
)

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Keep `go test ./...` usable without envtest binaries.
		os.Exit(0)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// ENVTEST_CRDS points to the real CRDs pinned by the chart (make
	// envtest-crds), so generated objects are validated against the real
	// schemas and CEL rules. The schema-less fallback only checks behaviour.
	crds := os.Getenv("ENVTEST_CRDS")
	if crds == "" {
		crds = filepath.Join("testdata", "crds")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crds},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	if err != nil {
		panic(err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic(err)
	}

	cfg := config.Default()
	cfg.GatewaySectionName = "https"
	cfg.ResyncPeriod = time.Hour
	cfg.NetworkPolicy = true
	cfg.NetworkPolicyPeers = `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"envoy-gateway-system"}}}]`
	// "auth" instead of the real default: the "kubeflow" namespace is reserved
	// for TestChartManifestsAreValid.
	cfg.ExtAuthServiceName = "oauth2-proxy"
	cfg.ExtAuthServiceNamespace = "auth"
	peers, err := cfg.Peers()
	if err != nil {
		panic(err)
	}
	reconciler := &controller.NotebookReconciler{
		Client: mgr.GetClient(),
		Resolver: &access.Resolver{
			Reader:                 mgr.GetClient(),
			Reviewer:               access.SubjectAccessReviewer{Client: mgr.GetClient()},
			Verb:                   cfg.AccessVerb,
			GroupsEnabled:          true,
			IgnoredSubjectPrefixes: cfg.IgnoredSubjectPrefixes,
		},
		Recorder: mgr.GetEventRecorder(routes.ManagerName),
		Config:   cfg,
		Peers:    peers,
	}
	if err := reconciler.SetupWithManager(mgr, 2); err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()
	k8s = mgr.GetClient()

	// The ext-auth service namespace: policies reference its Service, so it
	// exists by definition and hosts the ReferenceGrants.
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: authNamespace}}); err != nil {
		panic(err)
	}

	code := m.Run()
	cancel()
	_ = env.Stop()
	os.Exit(code)
}

func TestNotebookLifecycle(t *testing.T) {
	ctx := context.Background()
	mustCreate(t, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})

	// Role scoped to a single notebook name, so access is per-Notebook.
	mustCreate(t, ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "open-lab"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"kubeflow.org"}, Resources: []string{"notebooks"},
			ResourceNames: []string{"lab"}, Verbs: []string{"get"},
		}},
	})
	mustCreate(t, ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "open-other"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"kubeflow.org"}, Resources: []string{"notebooks"},
			ResourceNames: []string{"other"}, Verbs: []string{"get"},
		}},
	})
	mustCreate(t, ctx, binding("alice", "open-lab",
		rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice@example.com"},
		rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "ml-team"},
		rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: namespace},
	))
	mustCreate(t, ctx, binding("carol", "open-other",
		rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "carol@example.com"},
	))

	nb := notebook(namespace, "lab")
	mustCreate(t, ctx, nb)

	name := types.NamespacedName{Namespace: namespace, Name: routes.Name(namespace, "lab")}

	t.Run("route is created and owned by the notebook", func(t *testing.T) {
		route := waitFor(t, ctx, routes.HTTPRouteGVK, name, func(u *unstructured.Unstructured) bool { return true })
		owner := metav1.GetControllerOf(route)
		if owner == nil || owner.UID != nb.GetUID() {
			t.Fatalf("route owner = %v", owner)
		}
		rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
		matches, _, _ := unstructured.NestedSlice(rules[0].(map[string]any), "matches")
		if p, _, _ := unstructured.NestedString(matches[0].(map[string]any), "path", "value"); p != "/notebook/alice/lab" {
			t.Fatalf("path = %q", p)
		}
		parent, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
		if parent[0].(map[string]any)["sectionName"] != "https" {
			t.Fatalf("parentRefs = %v", parent)
		}
	})

	t.Run("policy is self-contained for the gateway filter order", func(t *testing.T) {
		policy := waitFor(t, ctx, routes.SecurityPolicyGVK, name, func(*unstructured.Unstructured) bool { return true })
		if _, ok := policy.Object["spec"].(map[string]any)["mergeType"]; ok {
			t.Fatal("mergeType must be absent")
		}
		backendRefs, _, _ := unstructured.NestedSlice(policy.Object, "spec", "extAuth", "http", "backendRefs")
		if len(backendRefs) != 1 {
			t.Fatalf("extAuth backendRefs = %v", backendRefs)
		}
		ref := backendRefs[0].(map[string]any)
		if ref["name"] != "oauth2-proxy" || ref["namespace"] != authNamespace || ref["port"] != int64(4180) {
			t.Fatalf("extAuth backendRef = %v", ref)
		}
		headers, _, _ := unstructured.NestedStringSlice(policy.Object, "spec", "extAuth", "http", "headersToBackend")
		if !reflect.DeepEqual(headers, []string{"authorization"}) {
			t.Fatalf("headersToBackend = %v", headers)
		}
		check, _, _ := unstructured.NestedStringSlice(policy.Object, "spec", "extAuth", "headersToExtAuth")
		if !reflect.DeepEqual(check, []string{"cookie", "x-forwarded-proto", "x-forwarded-host", "x-forwarded-for"}) {
			t.Fatalf("headersToExtAuth = %v", check)
		}
	})

	t.Run("reference grant lets the policy reach the ext-auth service", func(t *testing.T) {
		grant := waitFor(t, ctx, routes.ReferenceGrantGVK,
			types.NamespacedName{Namespace: authNamespace, Name: routes.ReferenceGrantName(namespace)},
			func(*unstructured.Unstructured) bool { return true })
		from, _, _ := unstructured.NestedSlice(grant.Object, "spec", "from")
		if len(from) != 1 || from[0].(map[string]any)["namespace"] != namespace {
			t.Fatalf("from = %v", from)
		}
		to, _, _ := unstructured.NestedSlice(grant.Object, "spec", "to")
		if len(to) != 1 {
			t.Fatalf("to = %v", to)
		}
		target := to[0].(map[string]any)
		if target["group"] != "" || target["kind"] != "Service" || target["name"] != "oauth2-proxy" {
			t.Fatalf("to = %v", target)
		}
	})

	t.Run("network policy guards the notebook port", func(t *testing.T) {
		np := waitFor(t, ctx, routes.NetworkPolicyGVK, name, func(*unstructured.Unstructured) bool { return true })
		if metav1.GetControllerOf(np) == nil {
			t.Fatal("network policy must be owned by the notebook")
		}
		sel, _, _ := unstructured.NestedStringMap(np.Object, "spec", "podSelector", "matchLabels")
		if sel["statefulset"] != "lab" {
			t.Fatalf("podSelector = %v", sel)
		}
	})

	t.Run("policy allows exactly the subjects RBAC allows for this notebook", func(t *testing.T) {
		waitForPrincipals(t, ctx, name, []string{"alice@example.com"}, []string{"ml-team"})
	})

	t.Run("new RoleBinding grants access", func(t *testing.T) {
		mustCreate(t, ctx, binding("bob", "open-lab",
			rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "bob@example.com"},
		))
		waitForPrincipals(t, ctx, name, []string{"alice@example.com", "bob@example.com"}, []string{"ml-team"})
	})

	t.Run("removing a RoleBinding revokes access", func(t *testing.T) {
		if err := k8s.Delete(ctx, binding("alice", "open-lab")); err != nil {
			t.Fatal(err)
		}
		waitForPrincipals(t, ctx, name, []string{"bob@example.com"}, nil)
	})

	t.Run("cluster-wide grants are honoured", func(t *testing.T) {
		mustCreate(t, ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "notebook-reader"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"kubeflow.org"}, Resources: []string{"notebooks"}, Verbs: []string{"get", "list"},
			}},
		})
		mustCreate(t, ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "platform-admins"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "notebook-reader"},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "platform-admins"},
				{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:authenticated"},
			},
		})
		waitForPrincipals(t, ctx, name, []string{"bob@example.com"}, []string{"platform-admins"})
	})

	t.Run("annotation changes update the route", func(t *testing.T) {
		patch := client.MergeFrom(nb.DeepCopy())
		nb.SetAnnotations(map[string]string{routes.AnnotationRewriteURI: "/"})
		if err := k8s.Patch(ctx, nb, patch); err != nil {
			t.Fatal(err)
		}
		waitFor(t, ctx, routes.HTTPRouteGVK, name, func(u *unstructured.Unstructured) bool {
			rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
			filters, _, _ := unstructured.NestedSlice(rules[0].(map[string]any), "filters")
			for _, f := range filters {
				if v, _, _ := unstructured.NestedString(f.(map[string]any), "urlRewrite", "path", "replacePrefixMatch"); v == "/" {
					return true
				}
			}
			return false
		})
	})

	t.Run("manual edits are reverted", func(t *testing.T) {
		policy := get(t, ctx, routes.SecurityPolicyGVK, name)
		if err := unstructured.SetNestedField(policy.Object, "Allow", "spec", "authorization", "defaultAction"); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Update(ctx, policy); err != nil {
			t.Fatal(err)
		}
		waitFor(t, ctx, routes.SecurityPolicyGVK, name, func(u *unstructured.Unstructured) bool {
			v, _, _ := unstructured.NestedString(u.Object, "spec", "authorization", "defaultAction")
			return v == "Deny"
		})
	})

	t.Run("deleting the last notebook removes the reference grant", func(t *testing.T) {
		if err := k8s.Delete(ctx, notebook(namespace, "lab")); err != nil {
			t.Fatal(err)
		}
		waitForAbsent(t, ctx, routes.ReferenceGrantGVK,
			types.NamespacedName{Namespace: authNamespace, Name: routes.ReferenceGrantName(namespace)})
	})
}

func TestNoReferenceGrantInExtAuthNamespace(t *testing.T) {
	ctx := context.Background()
	mustCreate(t, ctx, notebook(authNamespace, "nb"))

	// The notebook's own objects prove the reconcile ran; a grant for the
	// ext-auth namespace itself would still be wrong.
	waitFor(t, ctx, routes.HTTPRouteGVK,
		types.NamespacedName{Namespace: authNamespace, Name: routes.Name(authNamespace, "nb")},
		func(*unstructured.Unstructured) bool { return true })
	waitForAbsent(t, ctx, routes.ReferenceGrantGVK,
		types.NamespacedName{Namespace: authNamespace, Name: routes.ReferenceGrantName(authNamespace)})
}

func TestDoesNotTakeOverForeignReferenceGrant(t *testing.T) {
	ctx := context.Background()
	const ns = "grant-takeover"
	mustCreate(t, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})

	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(routes.ReferenceGrantGVK)
	foreign.SetNamespace(authNamespace)
	foreign.SetName(routes.ReferenceGrantName(ns))
	foreign.SetLabels(map[string]string{"owner": "someone-else"})
	foreign.Object["spec"] = map[string]any{
		"from": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "namespace": ns}},
		"to":   []any{map[string]any{"group": "", "kind": "Service", "name": "other"}},
	}
	mustCreate(t, ctx, foreign)

	mustCreate(t, ctx, notebook(ns, "nb"))

	// The reconciler refuses the grant first, so the policy is never created
	// and the foreign grant keeps its spec.
	time.Sleep(3 * time.Second)
	grant := get(t, ctx, routes.ReferenceGrantGVK, types.NamespacedName{Namespace: authNamespace, Name: foreign.GetName()})
	spec := grant.Object["spec"].(map[string]any)
	if to, _, _ := unstructured.NestedSlice(spec, "to"); to[0].(map[string]any)["name"] != "other" {
		t.Fatalf("foreign grant was modified: %v", spec)
	}
	if grant.GetLabels()["owner"] != "someone-else" {
		t.Fatalf("foreign grant labels were modified: %v", grant.GetLabels())
	}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(routes.SecurityPolicyGVK)
	err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: routes.Name(ns, "nb")}, policy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no policy, got err=%v", err)
	}

	// Removing the last notebook must not delete a foreign grant either: the
	// deletion reconcile sees the label and keeps the grant. A failed precondition
	// would be retried, so observing it after some time is stable either way.
	if err := k8s.Delete(ctx, notebook(ns, "nb")); err != nil {
		t.Fatal(err)
	}
	waitForAbsent(t, ctx, routes.NotebookGVK, types.NamespacedName{Namespace: ns, Name: "nb"})
	time.Sleep(2 * time.Second)
	grant = get(t, ctx, routes.ReferenceGrantGVK, types.NamespacedName{Namespace: authNamespace, Name: foreign.GetName()})
	if to, _, _ := unstructured.NestedSlice(grant.Object["spec"].(map[string]any), "to"); to[0].(map[string]any)["name"] != "other" {
		t.Fatalf("foreign grant was deleted or modified: %v", grant.Object)
	}
}

// TestNamespacesAPI checks the Jupyter web app namespaces endpoint against
// real SubjectAccessReviews: only namespaces where the user may list
// notebooks are returned.
func TestNamespacesAPI(t *testing.T) {
	ctx := context.Background()
	const ns = "namespaces-api"
	mustCreate(t, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	mustCreate(t, ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "list-notebooks"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"kubeflow.org"}, Resources: []string{"notebooks"}, Verbs: []string{"list"}}},
	})
	mustCreate(t, ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bob-can-list"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "list-notebooks"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "bob@example.com"}},
	})

	handler := &nsapi.Handler{
		Reader:     k8s,
		Reviewer:   access.SubjectAccessReviewer{Client: k8s},
		UserHeader: "kubeflow-userid",
		TTL:        time.Minute,
	}
	req := httptest.NewRequest(http.MethodGet, "/jupyter/api/namespaces", nil)
	req.Header.Set(handler.UserHeader, "bob@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Success    bool     `json:"success"`
		User       string   `json:"user"`
		Namespaces []string `json:"namespaces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Every other namespace (alice, auth, takeover, ...) has no `list
	// notebooks` RoleBinding for bob.
	if !got.Success || got.User != "bob@example.com" || !reflect.DeepEqual(got.Namespaces, []string{ns}) {
		t.Fatalf("response = %+v", got)
	}
}

func TestDoesNotTakeOverForeignRoute(t *testing.T) {
	ctx := context.Background()
	const ns = "takeover"
	mustCreate(t, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})

	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(routes.HTTPRouteGVK)
	foreign.SetNamespace(ns)
	foreign.SetName(routes.Name(ns, "nb"))
	foreign.Object["spec"] = map[string]any{"hostnames": []any{"foreign.example.com"}}
	mustCreate(t, ctx, foreign)

	mustCreate(t, ctx, notebook(ns, "nb"))

	// The policy is never created because the route apply fails first, and
	// the foreign route keeps its spec.
	time.Sleep(3 * time.Second)
	route := get(t, ctx, routes.HTTPRouteGVK, types.NamespacedName{Namespace: ns, Name: foreign.GetName()})
	if parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs"); len(parents) != 0 {
		t.Fatalf("foreign route was modified: %v", route.Object["spec"])
	}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(routes.SecurityPolicyGVK)
	err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: foreign.GetName()}, policy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no policy, got err=%v", err)
	}
}

// TestChartManifestsAreValid server-side dry-runs every object rendered by
// the Helm chart (CHART_RENDER) against the real CRD schemas.
func TestChartManifestsAreValid(t *testing.T) {
	path := os.Getenv("CHART_RENDER")
	if path == "" {
		t.Skip("CHART_RENDER not set (run make test-integration)")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx := context.Background()
	mustCreate(t, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kubeflow"}})

	dec := yamlutil.NewYAMLOrJSONDecoder(f, 4096)
	count := 0
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(u.Object) == 0 {
			continue
		}
		count++
		if err := k8s.Create(ctx, u, client.DryRunAll); err != nil {
			t.Errorf("%s %s/%s: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
	}
	if count == 0 {
		t.Fatal("no objects rendered")
	}
	t.Logf("validated %d rendered objects", count)
}

func notebook(ns, name string) *unstructured.Unstructured {
	nb := &unstructured.Unstructured{}
	nb.SetGroupVersionKind(routes.NotebookGVK)
	nb.SetNamespace(ns)
	nb.SetName(name)
	nb.Object["spec"] = map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": name, "image": "jupyter"}},
	}}}
	return nb
}

func binding(name, role string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role},
		Subjects:   subjects,
	}
}

func mustCreate(t *testing.T, ctx context.Context, obj client.Object) {
	t.Helper()
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func get(t *testing.T, ctx context.Context, gvk schema.GroupVersionKind, name types.NamespacedName) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := k8s.Get(ctx, name, u); err != nil {
		t.Fatalf("get %s %s: %v", gvk.Kind, name, err)
	}
	return u
}

func waitFor(t *testing.T, ctx context.Context, gvk schema.GroupVersionKind, name types.NamespacedName, ok func(*unstructured.Unstructured) bool) *unstructured.Unstructured {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		err := k8s.Get(ctx, name, u)
		if err == nil && ok(u) {
			return u
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s %s (last err: %v, object: %v)", gvk.Kind, name, err, u.Object["spec"])
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForAbsent fails if object is still present when the deadline passes. It
// only observes our own deletion paths, which run without garbage collection
// in envtest.
func waitForAbsent(t *testing.T, ctx context.Context, gvk schema.GroupVersionKind, name types.NamespacedName) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		err := k8s.Get(ctx, name, u)
		if apierrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s still exists after %v (last err: %v)", gvk.Kind, name, timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitForPrincipals(t *testing.T, ctx context.Context, name types.NamespacedName, users, groups []string) {
	t.Helper()
	waitFor(t, ctx, routes.SecurityPolicyGVK, name, func(u *unstructured.Unstructured) bool {
		gotUsers, gotGroups := claimValues(u, "allow-users"), claimValues(u, "allow-groups")
		return reflect.DeepEqual(gotUsers, users) && reflect.DeepEqual(gotGroups, groups)
	})
}

func claimValues(policy *unstructured.Unstructured, rule string) []string {
	rules, _, _ := unstructured.NestedSlice(policy.Object, "spec", "authorization", "rules")
	for _, r := range rules {
		m := r.(map[string]any)
		if m["name"] != rule {
			continue
		}
		claims, _, _ := unstructured.NestedSlice(m, "principal", "jwt", "claims")
		values, _, _ := unstructured.NestedStringSlice(claims[0].(map[string]any), "values")
		return values
	}
	return nil
}
