package routes

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DefaultContainerPort is the notebook port used by the upstream controller
// when the Notebook does not declare one.
const DefaultContainerPort = 8888

// ContainerPort returns the port the notebook server listens on, resolved
// exactly like the upstream notebook-controller's generateService.
func ContainerPort(notebook *unstructured.Unstructured) int32 {
	containers, _, _ := unstructured.NestedSlice(notebook.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return DefaultContainerPort
	}
	c, _ := containers[0].(map[string]any)
	ports, _, _ := unstructured.NestedSlice(c, "ports")
	if len(ports) == 0 {
		return DefaultContainerPort
	}
	p, _ := ports[0].(map[string]any)
	switch v := p["containerPort"].(type) {
	case int64:
		return int32(v)
	case float64:
		return int32(v)
	}
	return DefaultContainerPort
}

// NetworkPolicy renders a policy that lets only the given peers (the gateway
// proxies and the notebook-controller's culler) reach the notebook server
// port. Without Istio there is no mTLS, so this is what prevents in-cluster
// clients from calling the notebook Service directly and bypassing the
// per-notebook gateway authorization. All other ports stay open so that
// unrelated in-cluster traffic (e.g. Spark or Ray callbacks) keeps working.
//
// The pod selector uses the "statefulset" label set by the upstream
// notebook-controller on notebook pods.
func NetworkPolicy(namespace, notebook string, port int32, peers []networkingv1.NetworkPolicyPeer) (*unstructured.Unstructured, error) {
	if len(peers) == 0 {
		// An empty "from" would allow every source.
		return nil, fmt.Errorf("networkpolicy for %s/%s: no allowed peers configured", namespace, notebook)
	}
	tcp := corev1.ProtocolTCP
	portRule := func(from, to int32) networkingv1.NetworkPolicyPort {
		p := networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: ptr(intstr.FromInt32(from))}
		if to > from {
			p.EndPort = &to
		}
		return p
	}

	rules := []networkingv1.NetworkPolicyIngressRule{{
		From:  peers,
		Ports: []networkingv1.NetworkPolicyPort{portRule(port, port)},
	}}
	var others []networkingv1.NetworkPolicyPort
	if port > 1 {
		others = append(others, portRule(1, port-1))
	}
	if port < 65535 {
		others = append(others, portRule(port+1, 65535))
	}
	udp := corev1.ProtocolUDP
	others = append(others, networkingv1.NetworkPolicyPort{Protocol: &udp})
	rules = append(rules, networkingv1.NetworkPolicyIngressRule{Ports: others})

	np := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"statefulset": notebook}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     rules,
		},
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(np)
	if err != nil {
		return nil, fmt.Errorf("convert networkpolicy: %w", err)
	}
	u := newObject(NetworkPolicyGVK, namespace, notebook)
	u.Object["spec"] = obj["spec"]
	return u, nil
}

func ptr[T any](v T) *T { return &v }
