package nsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"notebook-route-controller/internal/access"
)

// countingReviewer records the SubjectAccessReviews it is asked and returns
// true only for the namespaces it was configured with.
type countingReviewer struct {
	allowed map[string]bool
	calls   atomic.Int64
}

func (r *countingReviewer) Allowed(_ context.Context, _ string, _ []string, attrs authorizationv1.ResourceAttributes) (bool, error) {
	r.calls.Add(1)
	if err, ok := r.allowed["error:"+attrs.Namespace]; ok && err {
		return false, fmt.Errorf("review failed in %s", attrs.Namespace)
	}
	return r.allowed[attrs.Namespace], nil
}

func newHandler(t *testing.T, reviewer access.Reviewer, objects ...client.Object) *Handler {
	t.Helper()
	s := fake.NewClientBuilder().WithObjects(objects...).Build()
	return &Handler{Reader: s, Reviewer: reviewer, UserHeader: "kubeflow-userid", TTL: time.Minute}
}

func namespace(name string, phase corev1.NamespacePhase) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	ns.Status.Phase = phase
	return ns
}

func call(t *testing.T, h *Handler, method, header string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/jupyter/api/namespaces", nil)
	if header != "" {
		req.Header.Set(h.UserHeader, header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("response body: %s (%v)", rec.Body.String(), err)
	}
	return r
}

func TestMethodNotAllowed(t *testing.T) {
	rec := call(t, newHandler(t, &countingReviewer{}), http.MethodPost, "alice@example.com")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if r := decode(t, rec); r["success"] != false || str(r, "log") == "" {
		t.Fatalf("response = %v", r)
	}
}

func TestUnauthorizedWithoutUserHeader(t *testing.T) {
	rec := call(t, newHandler(t, &countingReviewer{}), http.MethodGet, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if r := decode(t, rec); r["success"] != false || str(r, "log") == "" {
		t.Fatalf("response = %v", r)
	}
}

func TestFiltersByListNotebooksAccess(t *testing.T) {
	reviewer := &countingReviewer{allowed: map[string]bool{"team-a": true, "team-b": false}}
	h := newHandler(t, reviewer,
		namespace("team-a", corev1.NamespaceActive),
		namespace("team-b", corev1.NamespaceActive),
		namespace("terminating", corev1.NamespaceTerminating),
	)

	rec := call(t, h, http.MethodGet, "alice@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	r := decode(t, rec)
	if r["success"] != true || r["log"] != nil || str(r, "user") != "alice@example.com" {
		t.Fatalf("response = %v", r)
	}
	if got := namespaces(t, r); len(got) != 1 || got[0] != "team-a" {
		t.Fatalf("namespaces = %v", got)
	}
}

func TestEmptyListOfNamespaces(t *testing.T) {
	rec := call(t, newHandler(t, &countingReviewer{}), http.MethodGet, "alice@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"namespaces":[]`) {
		t.Fatalf("body = %s", body)
	}
	if got := namespaces(t, decode(t, rec)); len(got) != 0 {
		t.Fatalf("namespaces = %v", got)
	}
}

func TestSortedNamespaces(t *testing.T) {
	reviewer := &countingReviewer{allowed: map[string]bool{"zeta": true, "alpha": true, "mid": true}}
	h := newHandler(t, reviewer,
		namespace("zeta", corev1.NamespaceActive),
		namespace("alpha", corev1.NamespaceActive),
		namespace("mid", corev1.NamespaceActive),
	)
	rec := call(t, h, http.MethodGet, "u")
	if got := namespaces(t, decode(t, rec)); len(got) != 3 || got[0] != "alpha" || got[1] != "mid" || got[2] != "zeta" {
		t.Fatalf("namespaces = %v", got)
	}
}

func TestReviewErrorIsInternalError(t *testing.T) {
	reviewer := &countingReviewer{allowed: map[string]bool{"error:team-a": true}}
	rec := call(t, newHandler(t, reviewer, namespace("team-a", corev1.NamespaceActive)), http.MethodGet, "u")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	r := decode(t, rec)
	if r["success"] != false || str(r, "log") == "" || r["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("response = %v", r)
	}
}

func str(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func namespaces(t *testing.T, m map[string]any) []string {
	t.Helper()
	raw, ok := m["namespaces"].([]any)
	if !ok {
		t.Fatalf("namespaces = %v", m["namespaces"])
	}
	out := make([]string, 0, len(raw))
	for i := range raw {
		if s, ok := raw[i].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestCacheAvoidsRepeatedReviews(t *testing.T) {
	reviewer := &countingReviewer{allowed: map[string]bool{"team-a": true}}
	h := newHandler(t, reviewer, namespace("team-a", corev1.NamespaceActive))

	if rec := call(t, h, http.MethodGet, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := reviewer.calls.Load(); got != 1 {
		t.Fatalf("first request reviews = %d", got)
	}
	if rec := call(t, h, http.MethodGet, "alice"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := reviewer.calls.Load(); got != 1 {
		t.Fatalf("cached request re-reviewed (%d calls)", got)
	}
	// A different user is not served from alice's cache.
	call(t, h, http.MethodGet, "bob")
	if got := reviewer.calls.Load(); got != 2 {
		t.Fatalf("second user reviews = %d", got)
	}
}

func TestExpiredCacheIsRecomputed(t *testing.T) {
	reviewer := &countingReviewer{allowed: map[string]bool{"team-a": true}}
	h := newHandler(t, reviewer, namespace("team-a", corev1.NamespaceActive))
	h.TTL = time.Millisecond

	call(t, h, http.MethodGet, "alice")
	time.Sleep(5 * time.Millisecond)
	call(t, h, http.MethodGet, "alice")
	if got := reviewer.calls.Load(); got != 2 {
		t.Fatalf("expired cache was not recomputed (%d calls)", got)
	}
}
