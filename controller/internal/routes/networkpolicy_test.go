package routes

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestContainerPort(t *testing.T) {
	nb := &unstructured.Unstructured{Object: map[string]any{}}
	if got := ContainerPort(nb); got != DefaultContainerPort {
		t.Fatalf("default port = %d", got)
	}
	nb.Object["spec"] = map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": "c", "ports": []any{map[string]any{"containerPort": int64(8787)}}}},
	}}}
	if got := ContainerPort(nb); got != 8787 {
		t.Fatalf("declared port = %d", got)
	}
}

func TestNetworkPolicyRestrictsOnlyTheNotebookPort(t *testing.T) {
	peers := []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "envoy-gateway-system"}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "envoy"}},
	}}
	u, err := NetworkPolicy("alice", "lab", 8888, peers)
	if err != nil {
		t.Fatal(err)
	}
	if u.GetName() != "notebook-alice-lab" || u.GetKind() != "NetworkPolicy" {
		t.Fatalf("identity = %s %s", u.GetKind(), u.GetName())
	}
	var np networkingv1.NetworkPolicy
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &np); err != nil {
		t.Fatal(err)
	}
	if np.Spec.PodSelector.MatchLabels["statefulset"] != "lab" {
		t.Fatalf("pod selector = %v", np.Spec.PodSelector)
	}
	if len(np.Spec.Ingress) != 2 {
		t.Fatalf("ingress rules = %d", len(np.Spec.Ingress))
	}
	gated := np.Spec.Ingress[0]
	if len(gated.From) != 1 || len(gated.Ports) != 1 || gated.Ports[0].Port.IntVal != 8888 || gated.Ports[0].EndPort != nil {
		t.Fatalf("gated rule = %+v", gated)
	}
	open := np.Spec.Ingress[1]
	if len(open.From) != 0 {
		t.Fatalf("other ports must be open to all sources: %+v", open.From)
	}
	// TCP 1-8887, TCP 8889-65535, all UDP.
	if len(open.Ports) != 3 ||
		open.Ports[0].Port.IntVal != 1 || *open.Ports[0].EndPort != 8887 ||
		open.Ports[1].Port.IntVal != 8889 || *open.Ports[1].EndPort != 65535 ||
		string(*open.Ports[2].Protocol) != "UDP" || open.Ports[2].Port != nil {
		t.Fatalf("open ports = %+v", open.Ports)
	}
	assertSerialisable(t, u)
}

func TestNetworkPolicyRequiresPeers(t *testing.T) {
	if _, err := NetworkPolicy("ns", "nb", 8888, nil); err == nil {
		t.Fatal("expected error: an empty peer list would allow every source")
	}
}
