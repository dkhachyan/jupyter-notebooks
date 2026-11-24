package routes

import (
	"reflect"
	"strings"
	"testing"

	"notebook-route-controller/internal/config"
)

func TestReferenceGrantName(t *testing.T) {
	if got := ReferenceGrantName("alice"); got != "notebook-extauth-alice" {
		t.Fatalf("name = %q", got)
	}
	// A namespace at the name-length boundary produces an untruncated name.
	long := strings.Repeat("n", 46)
	if got := ReferenceGrantName(long); got != "notebook-extauth-"+long {
		t.Fatalf("name = %q", got)
	}
	tooLong := strings.Repeat("n", 56)
	got := ReferenceGrantName(tooLong)
	if len(got) != 63 || !strings.HasPrefix(got, "notebook-extauth-") {
		t.Fatalf("name = %q", got)
	}
	if again := ReferenceGrantName(tooLong); got != again {
		t.Fatal("name must be deterministic")
	}
}

func TestReferenceGrant(t *testing.T) {
	cfg := config.Default()
	cfg.ExtAuthServiceName = "oauth2-proxy"
	cfg.ExtAuthServiceNamespace = "auth"
	g := ReferenceGrant("alice", cfg)
	if g.GetName() != "notebook-extauth-alice" || g.GetNamespace() != "auth" {
		t.Fatalf("identity = %s/%s", g.GetNamespace(), g.GetName())
	}
	labels := g.GetLabels()
	if labels[LabelManagedBy] != ManagerName || labels[LabelNotebookNamespace] != "alice" {
		t.Fatalf("labels = %v", labels)
	}
	spec := g.Object["spec"].(map[string]any)
	wantFrom := []any{map[string]any{
		"group": "gateway.envoyproxy.io", "kind": "SecurityPolicy", "namespace": "alice",
	}}
	if !reflect.DeepEqual(spec["from"], wantFrom) {
		t.Fatalf("from = %v", spec["from"])
	}
	wantTo := []any{map[string]any{"group": "", "kind": "Service", "name": "oauth2-proxy"}}
	if !reflect.DeepEqual(spec["to"], wantTo) {
		t.Fatalf("to = %v", spec["to"])
	}
	assertSerialisable(t, g)
}
