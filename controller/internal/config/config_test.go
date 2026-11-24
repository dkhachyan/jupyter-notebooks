package config

import (
	"flag"
	"reflect"
	"testing"
	"time"
)

func TestBindParsesFlags(t *testing.T) {
	cfg := Default()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.Bind(fs)
	err := fs.Parse([]string{
		"--gateway-namespace=gw", "--gateway-name=main", "--gateway-section-name=https",
		"--hostnames=a.example.com, b.example.com,", "--groups-claim=",
		"--remove-request-headers=", "--ext-auth-service-name=authz", "--ext-auth-service-port=9000",
		"--ext-auth-headers-to-backend=authorization,x-user-id",
		"--ext-auth-headers-to-ext-auth=cookie,x-tenant", "--resync-period=1m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayNamespace != "gw" || cfg.GatewayName != "main" || cfg.GatewaySectionName != "https" {
		t.Fatalf("gateway = %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Hostnames, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("hostnames = %v", cfg.Hostnames)
	}
	if cfg.GroupsClaim != "" || len(cfg.RemoveRequestHeaders) != 0 || cfg.ResyncPeriod != time.Minute {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.ExtAuthServiceName != "authz" || cfg.ExtAuthServicePort != 9000 {
		t.Fatalf("ext-auth service = %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.ExtAuthHeadersToBackend, []string{"authorization", "x-user-id"}) {
		t.Fatalf("ext-auth headers = %v", cfg.ExtAuthHeadersToBackend)
	}
	if !reflect.DeepEqual(cfg.ExtAuthHeadersToExtAuth, []string{"cookie", "x-tenant"}) {
		t.Fatalf("ext-auth check headers = %v", cfg.ExtAuthHeadersToExtAuth)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	cases := map[string]func(*Config){
		"no gateway":  func(c *Config) { c.GatewayName = "" },
		"bad port":    func(c *Config) { c.ServicePort = 0 },
		"no provider": func(c *Config) { c.JWTProvider = "" },
		"unnamed provider": func(c *Config) {
			c.JWTProvider = `{"remoteJWKS":{"uri":"https://x/keys"}}`
		},
		"provider without keys": func(c *Config) { c.JWTProvider = `{"name":"kubeflow"}` },
		"no verb":               func(c *Config) { c.AccessVerb = "" },
		"no resync":             func(c *Config) { c.ResyncPeriod = 0 },
		"invalid header":        func(c *Config) { c.RemoveRequestHeaders = []string{"bad header"} },
		"no ext-auth name":      func(c *Config) { c.ExtAuthServiceName = "" },
		"no ext-auth namespace": func(c *Config) { c.ExtAuthServiceNamespace = "" },
		"bad ext-auth port":     func(c *Config) { c.ExtAuthServicePort = 65536 },
		"no ext-auth headers":   func(c *Config) { c.ExtAuthHeadersToBackend = nil },
		"bad ext-auth header":   func(c *Config) { c.ExtAuthHeadersToBackend = []string{"bad header"} },
		"no check headers":      func(c *Config) { c.ExtAuthHeadersToExtAuth = nil },
		"bad check header":      func(c *Config) { c.ExtAuthHeadersToExtAuth = []string{"bad header"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestPeers(t *testing.T) {
	c := Default()
	c.NetworkPolicy = true
	if err := c.Validate(); err == nil {
		t.Fatal("network policy without peers must be rejected")
	}
	c.NetworkPolicyPeers = `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"envoy-gateway-system"}}}]`
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	peers, err := c.Peers()
	if err != nil || len(peers) != 1 || peers[0].NamespaceSelector == nil {
		t.Fatalf("peers = %+v, err = %v", peers, err)
	}
	for _, bad := range []string{`[{}]`, `[{"podSelectr":{}}]`, `not json`} {
		c.NetworkPolicyPeers = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("peers %q must be rejected", bad)
		}
	}
}
