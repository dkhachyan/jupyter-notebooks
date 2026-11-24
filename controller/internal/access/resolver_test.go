package access

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeReviewer allows the listed "user:<name>" / "group:<name>" subjects and
// records every review it receives.
type fakeReviewer struct {
	allowed map[string]bool
	err     error
	seen    []string
	attrs   []authorizationv1.ResourceAttributes
}

func (f *fakeReviewer) Allowed(_ context.Context, user string, groups []string, attrs authorizationv1.ResourceAttributes) (bool, error) {
	key := "user:" + user
	if user == "" {
		key = "group:" + strings.Join(groups, ",")
	}
	f.seen = append(f.seen, key)
	f.attrs = append(f.attrs, attrs)
	return f.allowed[key], f.err
}

func subject(kind, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: kind, APIGroup: rbacv1.GroupName, Name: name}
}

func fixtures() *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "alice", Name: "namespaceAdmin"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kubeflow-admin"},
			Subjects: []rbacv1.Subject{
				subject(rbacv1.UserKind, "oidc:alice@example.com"),
				subject(rbacv1.UserKind, "oidc:alice@example.com"), // duplicate
				{Kind: rbacv1.ServiceAccountKind, Name: "default-editor", Namespace: "alice"},
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "alice", Name: "user-bob"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kubeflow-view"},
			Subjects: []rbacv1.Subject{
				subject(rbacv1.UserKind, "oidc:bob@example.com"),
				subject(rbacv1.UserKind, "unprefixed@example.com"),
				subject(rbacv1.GroupKind, "oidc:ml-team"),
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "carol"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kubeflow-admin"},
			Subjects:   []rbacv1.Subject{subject(rbacv1.UserKind, "oidc:carol@example.com")},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "admins"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{
				subject(rbacv1.GroupKind, "system:masters"),
				subject(rbacv1.GroupKind, "oidc:platform-admins"),
				subject(rbacv1.UserKind, "oidc:"),
			},
		},
	)
}

func newResolver(r Reviewer) *Resolver {
	return &Resolver{
		Reader:                 fixtures().Build(),
		Reviewer:               r,
		Verb:                   "get",
		UserPrefix:             "oidc:",
		GroupPrefix:            "oidc:",
		GroupsEnabled:          true,
		IgnoredSubjectPrefixes: []string{"system:"},
	}
}

func TestResolveChecksOnlyMappableSubjects(t *testing.T) {
	rev := &fakeReviewer{allowed: map[string]bool{
		"user:oidc:alice@example.com": true,
		"group:oidc:platform-admins":  true,
		// bob is bound but RBAC denies access to this notebook (e.g. resourceNames).
	}}
	p, err := newResolver(rev).Resolve(context.Background(), "alice", "lab")
	if err != nil {
		t.Fatal(err)
	}

	want := Principals{Users: []string{"alice@example.com"}, Groups: []string{"platform-admins"}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("principals = %+v, want %+v", p, want)
	}
	wantSeen := []string{
		"user:oidc:alice@example.com",
		"user:oidc:bob@example.com",
		"group:oidc:ml-team",
		"group:oidc:platform-admins",
	}
	if !reflect.DeepEqual(rev.seen, wantSeen) {
		t.Fatalf("reviewed %v, want %v (carol is bound in another namespace, system:/unprefixed/SA subjects are skipped)", rev.seen, wantSeen)
	}
	wantAttrs := authorizationv1.ResourceAttributes{
		Namespace: "alice", Verb: "get", Group: "kubeflow.org", Resource: "notebooks", Name: "lab",
	}
	for _, a := range rev.attrs {
		if a != wantAttrs {
			t.Fatalf("attrs = %+v, want %+v", a, wantAttrs)
		}
	}
}

func TestResolveGroupsDisabled(t *testing.T) {
	rev := &fakeReviewer{allowed: map[string]bool{"group:oidc:ml-team": true}}
	r := newResolver(rev)
	r.GroupsEnabled = false
	p, err := r.Resolve(context.Background(), "alice", "lab")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 0 {
		t.Fatalf("groups must not be resolved when disabled: %v", p.Groups)
	}
	for _, s := range rev.seen {
		if strings.HasPrefix(s, "group:") {
			t.Fatalf("unexpected group review %s", s)
		}
	}
}

func TestResolveFailsClosedOnReviewError(t *testing.T) {
	rev := &fakeReviewer{err: errors.New("apiserver unavailable")}
	if _, err := newResolver(rev).Resolve(context.Background(), "alice", "lab"); err == nil {
		t.Fatal("expected error")
	}
}
