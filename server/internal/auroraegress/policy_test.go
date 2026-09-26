package auroraegress

import (
	"context"
	"net"
	"net/url"
	"testing"
)

// mustPolicy returns a policy whose resolver answers with one public address
// for any host, so shape-level authorization tests stay offline.
func mustPolicy(t *testing.T) Policy {
	t.Helper()
	p, err := NewPolicy("https://multica.example.com", nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p.Resolve = staticResolver(net.ParseIP("93.184.216.34"))
	return p
}

// staticResolver builds a ResolveFunc returning ips in order.
func staticResolver(ips ...net.IP) ResolveFunc {
	return func(context.Context, string) ([]net.IPAddr, error) {
		addrs := make([]net.IPAddr, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, net.IPAddr{IP: ip})
		}
		return addrs, nil
	}
}

func policyResolvingTo(t *testing.T, ips ...net.IP) Policy {
	t.Helper()
	p := mustPolicy(t)
	p.Resolve = staticResolver(ips...)
	return p
}

func providerTarget() *url.URL {
	return &url.URL{Scheme: "https", Host: "api.anthropic.com:443"}
}

func TestPolicyAllowsCompiledProviderHTTPSHosts(t *testing.T) {
	p := mustPolicy(t)
	if len(CompiledProviderHosts) == 0 {
		t.Fatal("no compiled provider hosts")
	}
	want := map[string]bool{
		"api.anthropic.com:443":         false,
		"ark.cn-beijing.volces.com:443": false,
		"api.openai.com:443":            false,
		"openspeech.bytedance.com:443":  false,
	}
	for _, host := range CompiledProviderHosts {
		target := &url.URL{Scheme: "https", Host: host}
		if err := p.Authorize(target, true); err != nil {
			t.Errorf("Authorize(CONNECT %s) = %v, want nil", host, err)
		}
		if _, ok := want[host]; ok {
			want[host] = true
		}
	}
	for host, seen := range want {
		if !seen {
			t.Errorf("compiled provider host %s is missing from CompiledProviderHosts", host)
		}
	}
}

func TestPolicyAllowsExactConfiguredServerOrigin(t *testing.T) {
	p := mustPolicy(t)
	allowed := []struct {
		name      string
		target    *url.URL
		isConnect bool
	}{
		{"https origin without port", &url.URL{Scheme: "https", Host: "multica.example.com"}, true},
		{"https origin explicit 443", &url.URL{Scheme: "https", Host: "multica.example.com:443"}, true},
	}
	for _, tc := range allowed {
		if err := p.Authorize(tc.target, tc.isConnect); err != nil {
			t.Errorf("%s: Authorize = %v, want nil", tc.name, err)
		}
	}

	// An http origin is reached by forward proxy, not CONNECT.
	httpPolicy, err := NewPolicy("http://multica.internal:8080", nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	httpPolicy.Resolve = staticResolver(net.ParseIP("93.184.216.34"))
	if err := httpPolicy.Authorize(&url.URL{Scheme: "http", Host: "multica.internal:8080"}, false); err != nil {
		t.Errorf("exact http origin: Authorize = %v, want nil", err)
	}
	// The same host on a sibling port is not the origin.
	if err := httpPolicy.Authorize(&url.URL{Scheme: "http", Host: "multica.internal:8081"}, false); err == nil {
		t.Error("sibling port of the configured origin was allowed")
	}
	// A different host is not the origin, even on the configured port.
	if err := httpPolicy.Authorize(&url.URL{Scheme: "http", Host: "evil.example.com:8080"}, false); err == nil {
		t.Error("different host was allowed as the server origin")
	}
}

func TestPolicyRejectsHTTPForProviderHosts(t *testing.T) {
	p := mustPolicy(t)
	cases := []struct {
		target    *url.URL
		isConnect bool
	}{
		{&url.URL{Scheme: "http", Host: "api.anthropic.com"}, false},
		{&url.URL{Scheme: "http", Host: "api.anthropic.com:443"}, false},
		{&url.URL{Scheme: "https", Host: "api.anthropic.com:443"}, false},
		{&url.URL{Scheme: "http", Host: "api.anthropic.com:8080"}, true},
		{&url.URL{Scheme: "ftp", Host: "api.anthropic.com:443"}, true},
	}
	for _, tc := range cases {
		if err := p.Authorize(tc.target, tc.isConnect); err == nil {
			t.Errorf("Authorize(%s, %v) = nil, want rejection", tc.target, tc.isConnect)
		}
	}
}

func TestPolicyRejectsUnknownHostAndPort(t *testing.T) {
	p := mustPolicy(t)
	cases := []struct {
		target    *url.URL
		isConnect bool
	}{
		{&url.URL{Scheme: "https", Host: "evil.example.com:443"}, true},
		{&url.URL{Scheme: "https", Host: "api.anthropic.com:8443"}, true},
		{&url.URL{Scheme: "https", Host: "api.anthropic.com"}, true},
		{&url.URL{Scheme: "http", Host: "evil.example.com:80"}, false},
		{&url.URL{Scheme: "https", Host: "multica.example.com:8443"}, true},
	}
	for _, tc := range cases {
		if err := p.Authorize(tc.target, tc.isConnect); err == nil {
			t.Errorf("Authorize(%s, %v) = nil, want rejection", tc.target, tc.isConnect)
		}
	}
}

func TestPolicyRejectsURLCredentialsAndIPLiteral(t *testing.T) {
	p := mustPolicy(t)
	cases := []*url.URL{
		{Scheme: "https", Host: "api.anthropic.com:443", User: url.UserPassword("user", "pass")},
		{Scheme: "https", Host: "multica.example.com", User: url.User("user")},
		{Scheme: "https", Host: "10.0.0.1:443"},
		{Scheme: "https", Host: "127.0.0.1:443"},
		{Scheme: "https", Host: "[2001:db8::1]:443"},
		{Scheme: "https", Host: "[::1]:443"},
	}
	for _, target := range cases {
		if err := p.Authorize(target, true); err == nil {
			t.Errorf("Authorize(%s, true) = nil, want rejection", target)
		}
		if err := p.Authorize(target, false); err == nil {
			t.Errorf("Authorize(%s, false) = nil, want rejection", target)
		}
	}
}

func TestPolicyRejectsPrivateLoopbackLinkLocalCGNATAndMetadataIPs(t *testing.T) {
	ctx := context.Background()
	blocked := map[string]string{
		"loopback ipv4":       "127.0.0.1",
		"loopback ipv6":       "::1",
		"rfc1918 ten":         "10.20.30.40",
		"rfc1918 one seventy": "172.16.5.6",
		"rfc1918 one ninety":  "192.168.1.1",
		"link local ipv4":     "169.254.1.1",
		"metadata":            "169.254.169.254",
		"link local ipv6":     "fe80::1",
		"cgnat":               "100.64.0.1",
		"unique local ipv6":   "fd00::1",
		"multicast ipv4":      "224.0.0.1",
		"multicast ipv6":      "ff02::1",
		"documentation ipv4":  "192.0.2.10",
		"documentation ipv6":  "2001:db8::1",
		"benchmark ipv4":      "198.18.0.1",
		"benchmark ipv6":      "2001:2::1",
		"unspecified ipv4":    "0.0.0.0",
		"unspecified ipv6":    "::",
		"reserved":            "240.0.0.1",
		"this network":        "0.1.2.3",
		"protocol assignment": "192.0.0.9",
		"mapped private ipv4": "::ffff:10.0.0.1",
		"six to four":         "2002:0a00:0001::1",
		"nat64":               "64:ff9b::a00:1",
	}
	for name, ip := range blocked {
		p := policyResolvingTo(t, net.ParseIP(ip))
		if _, err := p.Validate(ctx, providerTarget(), true); err == nil {
			t.Errorf("%s: Validate(%s) = nil, want rejection", name, ip)
		}
	}

	// A fully public answer is accepted and returned in resolver order.
	p := policyResolvingTo(t, net.ParseIP("93.184.216.34"), net.ParseIP("2606:2800:220:1:248:1893:25c8:1946"))
	target, err := p.Validate(ctx, providerTarget(), true)
	if err != nil {
		t.Fatalf("public answer rejected: %v", err)
	}
	if len(target.Addrs) != 2 {
		t.Fatalf("got %d addresses, want 2", len(target.Addrs))
	}
}

func TestPolicyRejectsMixedPublicAndPrivateDNSAnswers(t *testing.T) {
	ctx := context.Background()
	p := policyResolvingTo(t, net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5"))
	if _, err := p.Validate(ctx, providerTarget(), true); err == nil {
		t.Fatal("mixed public/private answer was accepted")
	}

	// The server origin is the one exception: it may resolve privately, but
	// only for its exact host and port.
	originPolicy, err := NewPolicy("https://multica.internal", nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	originPolicy.Resolve = staticResolver(net.ParseIP("10.0.0.7"))
	if _, err := originPolicy.Validate(ctx, &url.URL{Scheme: "https", Host: "multica.internal"}, true); err != nil {
		t.Fatalf("server origin with private address rejected: %v", err)
	}
	// A sibling port is rejected even though it resolves to the same IP.
	if _, err := originPolicy.Validate(ctx, &url.URL{Scheme: "https", Host: "multica.internal:8443"}, true); err == nil {
		t.Fatal("sibling port of the server origin was allowed")
	}
	// Another host resolving to the same private address is rejected; the
	// private-address exception never generalizes.
	if _, err := originPolicy.Validate(ctx, &url.URL{Scheme: "http", Host: "evil.example.com:80"}, false); err == nil {
		t.Fatal("unrelated host resolving privately was allowed")
	}
}

func TestNewPolicyRejectsWildcardAndNonProviderPorts(t *testing.T) {
	cases := map[string]string{
		"wildcard host":     "*.anthropic.com:443",
		"wildcard port":     "api.anthropic.com:*",
		"non provider port": "api.anthropic.com:8443",
		"missing port":      "api.anthropic.com",
		"ip literal":        "10.0.0.1:443",
		"empty host":        ":443",
	}
	for name, host := range cases {
		if _, err := NewPolicy("https://multica.example.com", []string{host}); err == nil {
			t.Errorf("%s (%q): NewPolicy accepted an invalid extra host", name, host)
		}
	}
	for _, origin := range []string{"not a url", "https://multica.example.com/path", "https://user:pass@multica.example.com", "ftp://multica.example.com"} {
		if _, err := NewPolicy(origin, nil); err == nil {
			t.Errorf("NewPolicy accepted an invalid origin %q", origin)
		}
	}
	if _, err := NewPolicy("https://multica.example.com", []string{"cdn.example.com:443"}); err != nil {
		t.Errorf("NewPolicy rejected a valid extra host: %v", err)
	}
}
