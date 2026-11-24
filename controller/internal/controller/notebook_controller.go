// Package controller reconciles Notebooks into Gateway API HTTPRoutes, Envoy
// Gateway SecurityPolicies (self-contained extAuth + jwt + authorization) and
// the ReferenceGrants that let the policies reach the external authorization
// service.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"notebook-route-controller/internal/access"
	"notebook-route-controller/internal/config"
	"notebook-route-controller/internal/routes"
)

// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=securitypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// NotebookReconciler keeps one HTTPRoute and one self-contained SecurityPolicy
// (extAuth + jwt + authorization) per Notebook, plus a cross-namespace
// ReferenceGrant per notebook namespace that lets the policy's extAuth backend
// reference the external authorization Service.
type NotebookReconciler struct {
	Client   client.Client
	Resolver *access.Resolver
	Recorder events.EventRecorder
	Config   config.Config
	// Peers allowed to reach the notebook port; used when Config.NetworkPolicy is set.
	Peers []networkingv1.NetworkPolicyPeer
	// grantLocks serializes per-namespace ReferenceGrant changes, so that
	// deleting the grant of the last notebook never races with a new notebook
	// in the same namespace.
	grantLocks grantLocks
}

// Reconcile renders and server-side applies the route and policy for a
// Notebook, and keeps the namespace's ReferenceGrant for the external
// authorization service in sync.
func (r *NotebookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	nb := newNotebook()
	if err := r.Client.Get(ctx, req.NamespacedName, nb); err != nil {
		if apierrors.IsNotFound(err) {
			// The last Notebook of a namespace releases its ReferenceGrant;
			// generated objects are owned by the Notebook and garbage collected.
			return ctrl.Result{}, r.cleanupReferenceGrant(ctx, req.Namespace)
		}
		return ctrl.Result{}, err
	}
	if !nb.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, r.cleanupReferenceGrant(ctx, nb.GetNamespace())
	}

	if nb.GetNamespace() != r.Config.ExtAuthServiceNamespace {
		if err := r.applyReferenceGrant(ctx, nb.GetNamespace()); err != nil {
			r.Recorder.Eventf(nb, nil, corev1.EventTypeWarning, "ApplyFailed", "Reconcile", "ReferenceGrant: %v", err)
			return ctrl.Result{}, err
		}
	}

	principals, err := r.Resolver.Resolve(ctx, nb.GetNamespace(), nb.GetName())
	if err != nil {
		r.Recorder.Eventf(nb, nil, corev1.EventTypeWarning, "AccessReviewFailed", "Reconcile", "%v", err)
		return ctrl.Result{}, err
	}

	route, warnings := routes.HTTPRoute(nb.GetNamespace(), nb.GetName(), nb.GetAnnotations(), r.Config)
	for _, w := range warnings {
		r.Recorder.Eventf(nb, nil, corev1.EventTypeWarning, "InvalidAnnotation", "Reconcile", "%s", w)
	}
	policy, err := routes.SecurityPolicy(nb.GetNamespace(), nb.GetName(), principals, r.Config)
	if err != nil {
		return ctrl.Result{}, err
	}

	objects := []*unstructured.Unstructured{route, policy}
	if r.Config.NetworkPolicy {
		np, err := routes.NetworkPolicy(nb.GetNamespace(), nb.GetName(), routes.ContainerPort(nb), r.Peers)
		if err != nil {
			return ctrl.Result{}, err
		}
		objects = append(objects, np)
	}

	policyChanged := false
	for _, obj := range objects {
		if err := controllerutil.SetControllerReference(nb, obj, r.Client.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		changed, err := r.apply(ctx, obj)
		if err != nil {
			r.Recorder.Eventf(nb, nil, corev1.EventTypeWarning, "ApplyFailed", "Reconcile", "%s %s: %v", obj.GetKind(), obj.GetName(), err)
			return ctrl.Result{}, err
		}
		if changed {
			policyChanged = policyChanged || obj == policy
			logger.Info("applied", "kind", obj.GetKind(), "name", obj.GetName(),
				"users", len(principals.Users), "groups", len(principals.Groups))
			r.Recorder.Eventf(nb, nil, corev1.EventTypeNormal, "Configured", "Reconcile", "%s %s updated", obj.GetKind(), obj.GetName())
		}
	}
	if policyChanged && principals.Empty() {
		r.Recorder.Eventf(nb, nil, corev1.EventTypeWarning, "NoAccess", "Reconcile",
			"no RBAC subject may %s this notebook; all gateway requests are denied", r.Config.AccessVerb)
	}

	return ctrl.Result{RequeueAfter: r.Config.ResyncPeriod}, nil
}

// apply server-side applies obj and reports whether its rendered spec changed.
// The apply always runs so that manual edits to owned fields are reverted; a
// no-op apply does not modify the object. The rendered spec is fingerprinted
// in an annotation only to report changes, because the live object carries
// API-server defaults that make a direct comparison meaningless.
func (r *NotebookReconciler) apply(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	hash, err := specHash(obj)
	if err != nil {
		return false, err
	}
	obj.SetAnnotations(map[string]string{AnnotationSpecHash: hash})

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	err = r.Client.Get(ctx, client.ObjectKeyFromObject(obj), current)
	changed := true
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return false, err
	case !sameController(current, obj):
		return false, fmt.Errorf("refusing to take over %s %s/%s: not controlled by this Notebook",
			obj.GetKind(), obj.GetNamespace(), obj.GetName())
	default:
		changed = current.GetAnnotations()[AnnotationSpecHash] != hash
	}

	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
		client.FieldOwner(routes.ManagerName), client.ForceOwnership); err != nil {
		return false, err
	}
	return changed, nil
}

// applyReferenceGrant server-side applies the ReferenceGrant that lets the
// notebook SecurityPolicy in ns reference the external authorization Service.
// Unlike apply, the grant has no controller ownerRef: a cross-namespace owner
// reference is invalid, so the grant is shared by all notebooks of a namespace
// and deleted once the last one is gone.
func (r *NotebookReconciler) applyReferenceGrant(ctx context.Context, ns string) error {
	unlock := r.grantLocks.lock(ns)
	defer unlock()

	grant := routes.ReferenceGrant(ns, r.Config)
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(routes.ReferenceGrantGVK)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(grant), current)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case current.GetLabels()[routes.LabelManagedBy] != routes.ManagerName:
		return fmt.Errorf("refusing to take over ReferenceGrant %s/%s: not managed by this controller",
			grant.GetNamespace(), grant.GetName())
	}
	hash, err := specHash(grant)
	if err != nil {
		return err
	}
	grant.SetAnnotations(map[string]string{AnnotationSpecHash: hash})
	changed := current.GetAnnotations()[AnnotationSpecHash] != hash

	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(grant),
		client.FieldOwner(routes.ManagerName), client.ForceOwnership); err != nil {
		return err
	}
	if changed {
		log.FromContext(ctx).Info("applied", "kind", grant.GetKind(), "name", grant.GetName(), "namespace", ns)
	}
	return nil
}

// cleanupReferenceGrant deletes the namespace's ReferenceGrant once its last
// Notebook is gone. Notebooks that are themselves terminating do not count.
func (r *NotebookReconciler) cleanupReferenceGrant(ctx context.Context, ns string) error {
	if ns == r.Config.ExtAuthServiceNamespace {
		return nil
	}
	unlock := r.grantLocks.lock(ns)
	defer unlock()

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(routes.NotebookGVK.GroupVersion().WithKind("NotebookList"))
	if err := r.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range list.Items {
		if list.Items[i].GetDeletionTimestamp().IsZero() {
			return nil
		}
	}
	grant := routes.ReferenceGrant(ns, r.Config)
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(routes.ReferenceGrantGVK)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(grant), current)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case current.GetLabels()[routes.LabelManagedBy] != routes.ManagerName:
		log.FromContext(ctx).V(1).Info("keeping foreign ReferenceGrant", "name", grant.GetName(), "namespace", grant.GetNamespace())
		return nil
	}
	uid := current.GetUID()
	return client.IgnoreNotFound(r.Client.Delete(ctx, grant, client.Preconditions{UID: &uid}))
}

// grantLocks holds one mutex per namespace.
type grantLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (l *grantLocks) lock(ns string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*sync.Mutex{}
	}
	m := l.locks[ns]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[ns] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// AnnotationSpecHash stores the fingerprint of the last applied spec.
const AnnotationSpecHash = "notebooks.kubeflow.org/route-spec-hash"

func specHash(obj *unstructured.Unstructured) (string, error) {
	b, err := json.Marshal(obj.Object["spec"])
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// sameController reports whether existing is controlled by the owner that
// desired declares as its controller.
func sameController(existing, desired client.Object) bool {
	want := metav1.GetControllerOf(desired)
	got := metav1.GetControllerOf(existing)
	return want != nil && got != nil && want.UID == got.UID
}

// SetupWithManager registers the controller and its watches.
func (r *NotebookReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrent int) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routes.HTTPRouteGVK)
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(routes.SecurityPolicyGVK)

	namespaceNotebooks := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return r.notebookRequests(ctx, client.InNamespace(obj.GetNamespace()))
	})
	allNotebooks := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return r.notebookRequests(ctx)
	})

	b := ctrl.NewControllerManagedBy(mgr).
		Named("notebook-route").
		For(newNotebook(), builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Owns(route).
		Owns(policy)
	if r.Config.NetworkPolicy {
		b = b.Owns(&networkingv1.NetworkPolicy{})
	}
	return b.
		Watches(&rbacv1.RoleBinding{}, namespaceNotebooks).
		Watches(&rbacv1.Role{}, namespaceNotebooks).
		Watches(&rbacv1.ClusterRoleBinding{}, allNotebooks).
		Watches(&rbacv1.ClusterRole{}, allNotebooks).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}

func (r *NotebookReconciler) notebookRequests(ctx context.Context, opts ...client.ListOption) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(routes.NotebookGVK.GroupVersion().WithKind("NotebookList"))
	if err := r.Client.List(ctx, list, opts...); err != nil {
		log.FromContext(ctx).Error(err, "listing notebooks")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].GetNamespace(), Name: list.Items[i].GetName(),
		}})
	}
	return reqs
}

func newNotebook() *unstructured.Unstructured {
	nb := &unstructured.Unstructured{}
	nb.SetGroupVersionKind(routes.NotebookGVK)
	return nb
}
