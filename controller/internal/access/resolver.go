// Package access decides which identities may open a Notebook.
//
// Authorization is delegated to the Kubernetes API server: candidate subjects
// are collected from RoleBindings in the notebook namespace and from
// ClusterRoleBindings, and each candidate is checked with a
// SubjectAccessReview for "<verb> notebooks.kubeflow.org/<name>". Only
// subjects the API server allows end up in the gateway allow-list, so the
// result always matches what Kubernetes RBAC (or any other configured
// authorizer) says about that specific Notebook.
package access

import (
	"context"
	"fmt"
	"sort"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Principals are the JWT claim values allowed to access a notebook.
type Principals struct {
	Users  []string
	Groups []string
}

// Empty reports whether nobody is allowed.
func (p Principals) Empty() bool { return len(p.Users) == 0 && len(p.Groups) == 0 }

// Reviewer answers whether a user or group may perform an action.
type Reviewer interface {
	Allowed(ctx context.Context, user string, groups []string, attrs authorizationv1.ResourceAttributes) (bool, error)
}

// SubjectAccessReviewer implements Reviewer with SubjectAccessReviews.
type SubjectAccessReviewer struct {
	Client client.Client
}

// Allowed creates a SubjectAccessReview and returns its verdict.
func (r SubjectAccessReviewer) Allowed(ctx context.Context, user string, groups []string, attrs authorizationv1.ResourceAttributes) (bool, error) {
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               user,
			Groups:             groups,
			ResourceAttributes: &attrs,
		},
	}
	if err := r.Client.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("subjectaccessreview for user=%q groups=%v: %w", user, groups, err)
	}
	return sar.Status.Allowed && !sar.Status.Denied, nil
}

// Resolver computes the Principals allowed to access a notebook.
type Resolver struct {
	Reader   client.Reader
	Reviewer Reviewer

	Verb                   string
	UserPrefix             string
	GroupPrefix            string
	GroupsEnabled          bool
	IgnoredSubjectPrefixes []string
}

// Resolve returns the principals allowed to access notebook namespace/name.
func (r *Resolver) Resolve(ctx context.Context, namespace, name string) (Principals, error) {
	users, groups, err := r.candidates(ctx, namespace)
	if err != nil {
		return Principals{}, err
	}
	attrs := authorizationv1.ResourceAttributes{
		Namespace: namespace,
		Verb:      r.Verb,
		Group:     "kubeflow.org",
		Resource:  "notebooks",
		Name:      name,
	}

	var p Principals
	for _, u := range users {
		ok, err := r.Reviewer.Allowed(ctx, u, nil, attrs)
		if err != nil {
			return Principals{}, err
		}
		if ok {
			p.Users = append(p.Users, strings.TrimPrefix(u, r.UserPrefix))
		}
	}
	for _, g := range groups {
		ok, err := r.Reviewer.Allowed(ctx, "", []string{g}, attrs)
		if err != nil {
			return Principals{}, err
		}
		if ok {
			p.Groups = append(p.Groups, strings.TrimPrefix(g, r.GroupPrefix))
		}
	}
	return p, nil
}

// candidates returns the sorted, de-duplicated User and Group subjects bound
// in namespace or cluster-wide that can be mapped to JWT claims.
func (r *Resolver) candidates(ctx context.Context, namespace string) (users, groups []string, err error) {
	var rbs rbacv1.RoleBindingList
	if err := r.Reader.List(ctx, &rbs, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("list rolebindings in %s: %w", namespace, err)
	}
	var crbs rbacv1.ClusterRoleBindingList
	if err := r.Reader.List(ctx, &crbs); err != nil {
		return nil, nil, fmt.Errorf("list clusterrolebindings: %w", err)
	}

	userSet, groupSet := map[string]struct{}{}, map[string]struct{}{}
	add := func(subjects []rbacv1.Subject) {
		for _, s := range subjects {
			if s.APIGroup != rbacv1.GroupName || r.ignored(s.Name) {
				continue
			}
			switch s.Kind {
			case rbacv1.UserKind:
				if strings.HasPrefix(s.Name, r.UserPrefix) && len(s.Name) > len(r.UserPrefix) {
					userSet[s.Name] = struct{}{}
				}
			case rbacv1.GroupKind:
				if r.GroupsEnabled && strings.HasPrefix(s.Name, r.GroupPrefix) && len(s.Name) > len(r.GroupPrefix) {
					groupSet[s.Name] = struct{}{}
				}
			}
		}
	}
	for i := range rbs.Items {
		add(rbs.Items[i].Subjects)
	}
	for i := range crbs.Items {
		add(crbs.Items[i].Subjects)
	}
	return sortedKeys(userSet), sortedKeys(groupSet), nil
}

func (r *Resolver) ignored(name string) bool {
	for _, p := range r.IgnoredSubjectPrefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
