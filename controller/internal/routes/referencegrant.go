package routes

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"notebook-route-controller/internal/config"
)

var ReferenceGrantGVK = schema.GroupVersionKind{
	Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "ReferenceGrant",
}

// LabelNotebookNamespace marks the notebook namespace a ReferenceGrant was
// created for. It is also how the controller recognises its own grants.
const LabelNotebookNamespace = "notebooks.kubeflow.org/namespace"

// ReferenceGrantName returns the name of the ReferenceGrant for ns. It is
// truncated with a hash suffix so that the result is always a valid name.
func ReferenceGrantName(ns string) string {
	name := "notebook-extauth-" + ns
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(ns))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	base := strings.TrimRight(name[:63-len(suffix)], "-")
	return base + suffix
}

// ReferenceGrant renders the cross-namespace grant that lets the notebook
// SecurityPolicy in notebookNamespace reference the external authorization
// Service in cfg.ExtAuthServiceNamespace.
func ReferenceGrant(notebookNamespace string, cfg config.Config) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(ReferenceGrantGVK)
	u.SetNamespace(cfg.ExtAuthServiceNamespace)
	u.SetName(ReferenceGrantName(notebookNamespace))
	u.SetLabels(map[string]string{
		LabelManagedBy:         ManagerName,
		LabelNotebookNamespace: notebookNamespace,
	})
	u.Object["spec"] = map[string]any{
		"from": []any{map[string]any{
			"group":     SecurityPolicyGVK.Group,
			"kind":      SecurityPolicyGVK.Kind,
			"namespace": notebookNamespace,
		}},
		"to": []any{map[string]any{
			"group": "",
			"kind":  "Service",
			"name":  cfg.ExtAuthServiceName,
		}},
	}
	return u
}
