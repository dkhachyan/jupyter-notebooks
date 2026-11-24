// Package nsapi serves GET /jupyter/api/namespaces on behalf of the Jupyter
// web app: the upstream backend answers the namespace dropdown with every
// namespace the caller can list namespace-wide, which needs a cluster-wide
// `list namespaces` permission. This handler instead returns exactly the
// namespaces in which the user may `list notebooks.kubeflow.org` — the same
// check the web app performs when entering a namespace.
//
// The endpoint lives behind the web app's SecurityPolicy (ext_authz + jwt), so
// the trusted --namespaces-api-user-header carries the authenticated user.
package nsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"notebook-route-controller/internal/access"
)

// maxConcurrentReviews bounds the SubjectAccessReviews issued for one request.
const maxConcurrentReviews = 16

// Handler answers the Jupyter web app namespace list request.
type Handler struct {
	Reader     client.Reader
	Reviewer   access.Reviewer
	UserHeader string
	// TTL is how long a user's answer is cached.
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	namespaces []string
	expires    time.Time
}

// successResponse mirrors the fields of the upstream Jupyter web app backend.
type successResponse struct {
	Status     int      `json:"status"`
	Success    bool     `json:"success"`
	User       string   `json:"user"`
	Namespaces []string `json:"namespaces"`
}

type errorResponse struct {
	Success bool   `json:"success"`
	Log     string `json:"log"`
	Status  int    `json:"status"`
	User    string `json:"user"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(h.UserHeader)
	if r.Method != http.MethodGet {
		h.fail(w, user, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if user == "" {
		h.fail(w, user, http.StatusUnauthorized, fmt.Sprintf("%s header is empty", h.UserHeader))
		return
	}

	namespaces, err := h.namespaces(r.Context(), user)
	if err != nil {
		h.fail(w, user, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(successResponse{
		Status:     http.StatusOK,
		Success:    true,
		User:       user,
		Namespaces: namespaces,
	})
}

// namespaces returns the cached list for user or computes it.
func (h *Handler) namespaces(ctx context.Context, user string) ([]string, error) {
	now := time.Now()
	h.mu.Lock()
	cached, ok := h.cache[user]
	if ok && now.Before(cached.expires) {
		names := cached.namespaces
		h.mu.Unlock()
		return names, nil
	}
	// Drop expired entries while writing.
	if h.cache != nil {
		for u, e := range h.cache {
			if now.After(e.expires) {
				delete(h.cache, u)
			}
		}
	}
	h.mu.Unlock()

	var list corev1.NamespaceList
	if err := h.Reader.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	var active []string
	for i := range list.Items {
		if list.Items[i].Status.Phase == corev1.NamespaceActive {
			active = append(active, list.Items[i].GetName())
		}
	}

	allowedIn := make([]string, 0, len(active))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentReviews)
	var mu sync.Mutex
	for _, ns := range active {
		ns := ns
		g.Go(func() error {
			ok, err := h.Reviewer.Allowed(ctx, user, nil, authorizationv1.ResourceAttributes{
				Namespace: ns, Verb: "list", Group: "kubeflow.org", Resource: "notebooks",
			})
			if err == nil && ok {
				mu.Lock()
				allowedIn = append(allowedIn, ns)
				mu.Unlock()
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sort.Strings(allowedIn)

	h.mu.Lock()
	if h.cache == nil {
		h.cache = map[string]cacheEntry{}
	}
	h.cache[user] = cacheEntry{namespaces: allowedIn, expires: time.Now().Add(h.TTL)}
	h.mu.Unlock()
	return allowedIn, nil
}

func (h *Handler) fail(w http.ResponseWriter, user string, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Success: false,
		Log:     msg,
		Status:  code,
		User:    user,
	})
}
