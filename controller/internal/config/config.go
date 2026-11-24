// Package config holds the runtime configuration of the notebook route controller.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
)

// Config describes how routes and authorization policies are generated for
// Notebooks. Every field maps to a command-line flag (see Bind).
type Config struct {
	// Gateway the generated HTTPRoutes attach to.
	GatewayNamespace   string
	GatewayName        string
	GatewaySectionName string
	// Hostnames set on generated HTTPRoutes. Empty means the listener's hostnames.
	Hostnames []string

	// ServicePort is the port of the Service created by the upstream
	// notebook-controller (DefaultServingPort = 80 upstream).
	ServicePort int

	// ExtAuthService is the external authorization service (oauth2-proxy)
	// that performs OIDC login and returns an Authorization: Bearer header,
	// which the per-notebook SecurityPolicy's extAuth backendRefs point to.
	// Its namespace also hosts the cross-namespace ReferenceGrants.
	ExtAuthServiceName      string
	ExtAuthServiceNamespace string
	ExtAuthServicePort      int
	// ExtAuthHeadersToBackend are the ext-auth response headers forwarded to
	// the notebook (the session's ID token arrives as "authorization").
	ExtAuthHeadersToBackend []string
	// ExtAuthHeadersToExtAuth are the client request headers Envoy forwards to
	// the ext-auth service. Envoy's HTTP ext_authz default set does not
	// include the cookie, so without these the session is lost and login
	// loops.
	ExtAuthHeadersToExtAuth []string

	// JWTProvider is the JSON form of the Envoy Gateway JWTProvider that
	// validates the ID token forwarded by the ext-auth service. It is embedded
	// in every per-notebook SecurityPolicy: jwt principals require the policy
	// to define its provider (CRD validation).
	JWTProvider string
	// UserClaim and GroupsClaim are the JWT claims matched against the
	// Kubernetes RBAC subjects that may access a notebook. GroupsClaim may be
	// empty to disable group-based rules.
	UserClaim   string
	GroupsClaim string
	// UserPrefix and GroupPrefix are the prefixes the API server's OIDC
	// authenticator adds to usernames and groups (--oidc-username-prefix,
	// --oidc-groups-prefix). Only RBAC subjects carrying the prefix are
	// considered and the prefix is stripped before matching claims.
	UserPrefix  string
	GroupPrefix string
	// IgnoredSubjectPrefixes lists RBAC subject name prefixes that are never
	// turned into gateway principals (e.g. "system:").
	IgnoredSubjectPrefixes []string

	// AccessVerb is the verb checked on notebooks.kubeflow.org/<name> with a
	// SubjectAccessReview to decide whether a subject may open a notebook.
	AccessVerb string

	// NamespacesAPI serves GET /jupyter/api/namespaces for the Jupyter web
	// app when BindAddress is set. It answers with the namespaces in which the
	// user of UserHeader may list notebooks.kubeflow.org, cached for TTL.
	NamespacesAPIBindAddress string
	NamespacesAPIUserHeader  string
	NamespacesAPICacheTTL    time.Duration

	// RemoveRequestHeaders are removed by the HTTPRoute before the request
	// reaches the notebook (e.g. the client-supplied Authorization header,
	// which the ext-auth service must never trust).
	RemoveRequestHeaders []string

	// NetworkPolicy enables a per-notebook NetworkPolicy that only lets
	// NetworkPolicyPeers reach the notebook server port.
	NetworkPolicy bool
	// NetworkPolicyPeers is a JSON list of networking.k8s.io/v1
	// NetworkPolicyPeer objects (gateway proxies, notebook-controller culler).
	NetworkPolicyPeers string

	// ResyncPeriod re-evaluates access for every notebook periodically, which
	// catches RBAC changes that are not observable through watches.
	ResyncPeriod time.Duration
}

// Default returns the configuration used when no flags are set.
func Default() Config {
	return Config{
		GatewayNamespace:        "kubeflow",
		GatewayName:             "kubeflow-gateway",
		ExtAuthServiceName:      "oauth2-proxy",
		ExtAuthServiceNamespace: "kubeflow",
		ExtAuthServicePort:      4180,
		ExtAuthHeadersToBackend: []string{"authorization"},
		ExtAuthHeadersToExtAuth: []string{"cookie", "x-forwarded-proto", "x-forwarded-host", "x-forwarded-for"},
		ServicePort:             80,
		JWTProvider:             `{"name":"kubeflow","remoteJWKS":{"uri":"https://issuer.invalid/keys"}}`,
		UserClaim:               "email",
		GroupsClaim:             "groups",
		IgnoredSubjectPrefixes:  []string{"system:"},
		AccessVerb:              "get",
		NamespacesAPIUserHeader: "kubeflow-userid",
		NamespacesAPICacheTTL:   30 * time.Second,
		RemoveRequestHeaders:    []string{"authorization"},
		ResyncPeriod:            10 * time.Minute,
	}
}

// Bind registers flags for every field of c on fs, using the current values
// of c as defaults.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.GatewayNamespace, "gateway-namespace", c.GatewayNamespace, "Namespace of the Gateway the HTTPRoutes attach to.")
	fs.StringVar(&c.GatewayName, "gateway-name", c.GatewayName, "Name of the Gateway the HTTPRoutes attach to.")
	fs.StringVar(&c.GatewaySectionName, "gateway-section-name", c.GatewaySectionName, "Optional Gateway listener name the HTTPRoutes attach to.")
	fs.Var(stringList{&c.Hostnames}, "hostnames", "Comma-separated hostnames set on HTTPRoutes (empty: inherit from the listener).")
	fs.IntVar(&c.ServicePort, "service-port", c.ServicePort, "Port of the notebook Service created by the notebook-controller.")
	fs.StringVar(&c.ExtAuthServiceName, "ext-auth-service-name", c.ExtAuthServiceName, "Name of the external authorization service (oauth2-proxy).")
	fs.StringVar(&c.ExtAuthServiceNamespace, "ext-auth-service-namespace", c.ExtAuthServiceNamespace, "Namespace of the external authorization service (also hosts the cross-namespace ReferenceGrants).")
	fs.IntVar(&c.ExtAuthServicePort, "ext-auth-service-port", c.ExtAuthServicePort, "Port of the external authorization service.")
	fs.Var(stringList{&c.ExtAuthHeadersToBackend}, "ext-auth-headers-to-backend", "Comma-separated ext-auth response headers forwarded to the notebook.")
	fs.Var(stringList{&c.ExtAuthHeadersToExtAuth}, "ext-auth-headers-to-ext-auth", "Comma-separated client request headers included in the ext-auth check request (the session cookie must be among them).")
	fs.StringVar(&c.JWTProvider, "jwt-provider", c.JWTProvider, "JSON Envoy Gateway JWTProvider embedded in every per-notebook SecurityPolicy. It validates the ID token forwarded by the external authorization service.")
	fs.StringVar(&c.UserClaim, "user-claim", c.UserClaim, "JWT claim holding the user name matched against RBAC User subjects.")
	fs.StringVar(&c.GroupsClaim, "groups-claim", c.GroupsClaim, "JWT claim holding group names matched against RBAC Group subjects (empty disables group rules).")
	fs.StringVar(&c.UserPrefix, "user-prefix", c.UserPrefix, "Prefix of RBAC User subjects that map to JWT users (API server --oidc-username-prefix).")
	fs.StringVar(&c.GroupPrefix, "group-prefix", c.GroupPrefix, "Prefix of RBAC Group subjects that map to JWT groups (API server --oidc-groups-prefix).")
	fs.Var(stringList{&c.IgnoredSubjectPrefixes}, "ignored-subject-prefixes", "Comma-separated RBAC subject prefixes never exposed as gateway principals.")
	fs.StringVar(&c.AccessVerb, "access-verb", c.AccessVerb, "Verb on notebooks.kubeflow.org checked with SubjectAccessReview to grant access.")
	fs.StringVar(&c.NamespacesAPIBindAddress, "namespaces-api-bind-address", c.NamespacesAPIBindAddress, "Address serving GET /jupyter/api/namespaces for the Jupyter web app (empty disables it).")
	fs.StringVar(&c.NamespacesAPIUserHeader, "namespaces-api-user-header", c.NamespacesAPIUserHeader, "Request header carrying the authenticated user (set by the web app's ext_authz+jwt SecurityPolicy).")
	fs.DurationVar(&c.NamespacesAPICacheTTL, "namespaces-api-cache-ttl", c.NamespacesAPICacheTTL, "How long a user's namespace list answer is cached.")
	fs.Var(stringList{&c.RemoveRequestHeaders}, "remove-request-headers", "Comma-separated request headers removed before reaching the notebook.")
	fs.BoolVar(&c.NetworkPolicy, "network-policy", c.NetworkPolicy, "Create a NetworkPolicy per notebook restricting access to the notebook port.")
	fs.StringVar(&c.NetworkPolicyPeers, "network-policy-peers", c.NetworkPolicyPeers, "JSON list of NetworkPolicyPeers allowed to reach the notebook port.")
	fs.DurationVar(&c.ResyncPeriod, "resync-period", c.ResyncPeriod, "Period after which each notebook's access list is re-evaluated.")
}

var headerName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-.^_` + "`" + `|~]+$`)

// Validate reports configuration errors.
func (c Config) Validate() error {
	var errs []error
	if c.GatewayName == "" || c.GatewayNamespace == "" {
		errs = append(errs, errors.New("--gateway-name and --gateway-namespace are required"))
	}
	if c.ServicePort < 1 || c.ServicePort > 65535 {
		errs = append(errs, fmt.Errorf("--service-port %d out of range", c.ServicePort))
	}
	if c.ExtAuthServiceName == "" || c.ExtAuthServiceNamespace == "" {
		errs = append(errs, errors.New("--ext-auth-service-name and --ext-auth-service-namespace are required"))
	}
	if c.ExtAuthServicePort < 1 || c.ExtAuthServicePort > 65535 {
		errs = append(errs, fmt.Errorf("--ext-auth-service-port %d out of range", c.ExtAuthServicePort))
	}
	for _, list := range []struct {
		flag   string
		values []string
	}{
		{"--ext-auth-headers-to-backend", c.ExtAuthHeadersToBackend},
		{"--ext-auth-headers-to-ext-auth", c.ExtAuthHeadersToExtAuth},
	} {
		switch {
		case len(list.values) == 0:
			errs = append(errs, fmt.Errorf("%s must not be empty", list.flag))
		default:
			for _, h := range list.values {
				if !ValidHeaderName(h) {
					errs = append(errs, fmt.Errorf("%s: invalid header name %q", list.flag, h))
				}
			}
		}
	}
	if _, err := c.Provider(); err != nil {
		errs = append(errs, err)
	}
	if c.UserClaim == "" {
		errs = append(errs, errors.New("--user-claim is required"))
	}
	if c.AccessVerb == "" {
		errs = append(errs, errors.New("--access-verb is required"))
	}
	if name := c.NamespacesAPIBindAddress; name != "" {
		if !ValidHeaderName(c.NamespacesAPIUserHeader) {
			errs = append(errs, fmt.Errorf("--namespaces-api-user-header: invalid header name %q", c.NamespacesAPIUserHeader))
		}
		if c.NamespacesAPICacheTTL <= 0 {
			errs = append(errs, errors.New("--namespaces-api-cache-ttl must be positive"))
		}
	}
	if c.ResyncPeriod <= 0 {
		errs = append(errs, errors.New("--resync-period must be positive"))
	}
	for _, h := range c.RemoveRequestHeaders {
		if !headerName.MatchString(h) {
			errs = append(errs, fmt.Errorf("--remove-request-headers: invalid header name %q", h))
		}
	}
	if c.NetworkPolicy {
		peers, err := c.Peers()
		switch {
		case err != nil:
			errs = append(errs, err)
		case len(peers) == 0:
			errs = append(errs, errors.New("--network-policy requires at least one --network-policy-peers entry"))
		}
	}
	return errors.Join(errs...)
}

// Provider parses JWTProvider and returns it as an unstructured object.
func (c Config) Provider() (map[string]any, error) {
	var p map[string]any
	if err := json.Unmarshal([]byte(c.JWTProvider), &p); err != nil {
		return nil, fmt.Errorf("--jwt-provider: %w", err)
	}
	if name, _ := p["name"].(string); name == "" {
		return nil, errors.New("--jwt-provider: name is required")
	}
	if p["remoteJWKS"] == nil && p["localJWKS"] == nil {
		return nil, errors.New("--jwt-provider: remoteJWKS or localJWKS is required")
	}
	return p, nil
}

// Peers parses NetworkPolicyPeers. Unknown fields are rejected so that a
// typo cannot silently widen a peer to "everything".
func (c Config) Peers() ([]networkingv1.NetworkPolicyPeer, error) {
	if c.NetworkPolicyPeers == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(c.NetworkPolicyPeers))
	dec.DisallowUnknownFields()
	var peers []networkingv1.NetworkPolicyPeer
	if err := dec.Decode(&peers); err != nil {
		return nil, fmt.Errorf("--network-policy-peers: %w", err)
	}
	for i, p := range peers {
		if p.PodSelector == nil && p.NamespaceSelector == nil && p.IPBlock == nil {
			return nil, fmt.Errorf("--network-policy-peers[%d]: empty peer", i)
		}
	}
	return peers, nil
}

// ValidHeaderName reports whether name is a valid HTTP header name.
func ValidHeaderName(name string) bool { return headerName.MatchString(name) }

type stringList struct{ target *[]string }

func (s stringList) String() string {
	if s.target == nil {
		return ""
	}
	return strings.Join(*s.target, ",")
}

func (s stringList) Set(v string) error {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	*s.target = out
	return nil
}
