package auroraegress

import (
	"context"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"testing"
)

// mustPolicy returns a policy whose resolver answers with one public address
// for any host, so shape-level authorization tests stay offline.
func mustPolicy(t *testing.T) Policy {
	t.Helper()
	p, err := NewPolicy("https://multica.example.com", nil, nil)
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
	httpPolicy, err := NewPolicy("http://multica.internal:8080", nil, nil)
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
	originPolicy, err := NewPolicy("https://multica.internal", nil, nil)
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
		if _, err := NewPolicy("https://multica.example.com", []string{host}, nil); err == nil {
			t.Errorf("%s (%q): NewPolicy accepted an invalid extra host", name, host)
		}
	}
	for _, origin := range []string{"not a url", "https://multica.example.com/path", "https://user:pass@multica.example.com", "ftp://multica.example.com"} {
		if _, err := NewPolicy(origin, nil, nil); err == nil {
			t.Errorf("NewPolicy accepted an invalid origin %q", origin)
		}
	}
	if _, err := NewPolicy("https://multica.example.com", []string{"cdn.example.com:443"}, nil); err != nil {
		t.Errorf("NewPolicy rejected a valid extra host: %v", err)
	}
}

// providerTargetHost builds a CONNECT target for one host on port 443.
func providerTargetHost(host string) *url.URL {
	return &url.URL{Scheme: "https", Host: net.JoinHostPort(host, "443")}
}

// TestPolicyUsesPinnedAddressesWithoutResolution proves an operator pin replaces
// DNS only for an already-allowed provider host: the resolver is never called
// for the pinned host and the returned addresses are exactly the pinned ones,
// while an allowed host without a pin still resolves.
func TestPolicyUsesPinnedAddressesWithoutResolution(t *testing.T) {
	pins := map[string][]string{"ark.cn-beijing.volces.com": {"180.184.47.154", "2606:4700::1111"}}
	p, err := NewPolicy("https://multica.example.com", nil, pins)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	resolved := 0
	p.Resolve = func(context.Context, string) ([]net.IPAddr, error) {
		resolved++
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	target, err := p.Validate(context.Background(), providerTargetHost("ark.cn-beijing.volces.com"), true)
	if err != nil {
		t.Fatalf("pinned target refused: %v", err)
	}
	if resolved != 0 {
		t.Fatal("pinned host was resolved")
	}
	if target.Host != "ark.cn-beijing.volces.com:443" || len(target.Addrs) != 2 ||
		target.Addrs[0].IP.String() != "180.184.47.154" || target.Addrs[1].IP.String() != "2606:4700::1111" {
		t.Fatalf("pinned target = %+v", target)
	}
	// The same policy still resolves an allowed host that carries no pin.
	if _, err := p.Validate(context.Background(), providerTargetHost("api.anthropic.com"), true); err != nil {
		t.Fatalf("unpinned allowed host refused: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("unpinned host resolutions = %d, want 1", resolved)
	}
}

// TestNewPolicyRejectsUnsafeEgressPins pins the fail-closed pin matrix: a pin
// can never add a host, address an IP literal, carry a port, point at a
// non-public address, exceed the fixed bound, or be empty.
func TestNewPolicyRejectsUnsafeEgressPins(t *testing.T) {
	tooMany := make([]string, MaxEgressPinAddresses+1)
	for i := range tooMany {
		tooMany[i] = "93.184.216." + strconv.Itoa(34+i)
	}
	cases := map[string]map[string][]string{
		"unknown host":           {"evil.example.com": {"93.184.216.34"}},
		"non-public benchmark":   {"api.anthropic.com": {"198.18.0.5"}},
		"non-public rfc1918":     {"api.anthropic.com": {"10.0.0.1"}},
		"non-public loopback":    {"api.anthropic.com": {"127.0.0.1"}},
		"non-public ipv6":        {"api.anthropic.com": {"fe80::1"}},
		"host with port":         {"api.anthropic.com:443": {"93.184.216.34"}},
		"ip literal host":        {"93.184.216.34": {"93.184.216.34"}},
		"ipv6 literal host":      {"2606:4700::1111": {"2606:4700::1111"}},
		"uppercase host":         {"API.anthropic.com": {"93.184.216.34"}},
		"wildcard host":          {"*.anthropic.com": {"93.184.216.34"}},
		"empty list":             {"api.anthropic.com": {}},
		"too many addresses":     {"api.anthropic.com": tooMany},
		"malformed address":      {"api.anthropic.com": {"not-an-ip"}},
		"address with port":      {"api.anthropic.com": {"93.184.216.34:443"}},
		"bracketed ipv6 address": {"api.anthropic.com": {"[2606:4700::1111]"}},
	}
	for name, pins := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPolicy("https://multica.example.com", nil, pins); err == nil {
				t.Fatalf("NewPolicy accepted unsafe pins %v", pins)
			}
		})
	}
	// A public pin for an operator-added egress host is allowed, and a public
	// IPv6 pin is accepted.
	if _, err := NewPolicy("https://multica.example.com", []string{"cdn.example.com:443"}, map[string][]string{"cdn.example.com": {"2606:4700::1111"}}); err != nil {
		t.Fatalf("public pin for an extra allowed host rejected: %v", err)
	}
}

// TestPinnedTargetStillEnforcesCoreRules proves a pin never bypasses the
// allowlist, the https/443 rule, the no-IP-literal rule, or the non-public
// check that Validate re-applies to an injected policy.
func TestPinnedTargetStillEnforcesCoreRules(t *testing.T) {
	ctx := context.Background()
	p, err := NewPolicy("https://multica.example.com", nil, map[string][]string{"api.anthropic.com": {"93.184.216.34"}})
	if err != nil {
		t.Fatal(err)
	}
	p.Resolve = staticResolver(net.ParseIP("93.184.216.34"))
	refused := []struct {
		name      string
		target    *url.URL
		isConnect bool
	}{
		{"plain HTTP provider", &url.URL{Scheme: "https", Host: "api.anthropic.com:443"}, false},
		{"http scheme", &url.URL{Scheme: "http", Host: "api.anthropic.com:443"}, true},
		{"wrong port", &url.URL{Scheme: "https", Host: "api.anthropic.com:8443"}, true},
		{"missing explicit port", &url.URL{Scheme: "https", Host: "api.anthropic.com"}, true},
		{"pinned host as IP literal", &url.URL{Scheme: "https", Host: "93.184.216.34:443"}, true},
		{"credentials", &url.URL{Scheme: "https", Host: "api.anthropic.com:443", User: url.User("u")}, true},
	}
	for _, tc := range refused {
		if err := p.Authorize(tc.target, tc.isConnect); err == nil {
			t.Errorf("%s: pinned target was allowed", tc.name)
		}
	}
	key := "api.anthropic.com:443"
	// A directly injected non-public pin is refused by Validate.
	bad := Policy{AllowedTLSHosts: map[string]struct{}{key: {}}, Pins: map[string][]net.IPAddr{key: {{IP: net.ParseIP("10.0.0.1")}}}, Resolve: staticResolver(net.ParseIP("10.0.0.1"))}
	if _, err := bad.Validate(ctx, providerTargetHost("api.anthropic.com"), true); err == nil {
		t.Fatal("Validate accepted a non-public pin")
	}
	// An empty pin is refused.
	empty := Policy{AllowedTLSHosts: map[string]struct{}{key: {}}, Pins: map[string][]net.IPAddr{key: {}}, Resolve: staticResolver()}
	if _, err := empty.Validate(ctx, providerTargetHost("api.anthropic.com"), true); err == nil {
		t.Fatal("Validate accepted an empty pin")
	}
}

// TestPinsNeverOverrideTheServerOriginException proves the exact origin is still
// reached by name (and may resolve privately) when a pin names the same host.
func TestPinsNeverOverrideTheServerOriginException(t *testing.T) {
	p, err := NewPolicy("https://api.anthropic.com", nil, map[string][]string{"api.anthropic.com": {"93.184.216.34"}})
	if err != nil {
		t.Fatal(err)
	}
	p.Resolve = staticResolver(net.ParseIP("10.0.0.7"))
	target, err := p.Validate(context.Background(), &url.URL{Scheme: "https", Host: "api.anthropic.com"}, true)
	if err != nil || !target.Server || len(target.Addrs) != 1 || target.Addrs[0].IP.String() != "10.0.0.7" {
		t.Fatalf("server origin exception = %+v err=%v", target, err)
	}
}

// TestFormatParsePinsRoundTrip pins the deterministic sidecar wire format and
// the structural parse failures that must fail closed.
func TestFormatParsePinsRoundTrip(t *testing.T) {
	in := map[string][]string{
		"api.openai.com":            {"160.79.104.10"},
		"ark.cn-beijing.volces.com": {"180.184.47.154", "2606:4700::1111"},
		"api.anthropic.com":         {"160.79.104.10"},
	}
	want := "api.anthropic.com=160.79.104.10;api.openai.com=160.79.104.10;ark.cn-beijing.volces.com=180.184.47.154|2606:4700::1111"
	if got := FormatPins(in); got != want {
		t.Fatalf("FormatPins = %q, want %q", got, want)
	}
	parsed, err := ParsePins(want)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, in) {
		t.Fatalf("ParsePins = %v, want %v", parsed, in)
	}
	if FormatPins(nil) != "" || FormatPins(map[string][]string{}) != "" {
		t.Fatal("an empty pin map must render empty")
	}
	if parsed, err := ParsePins("   "); err != nil || parsed != nil {
		t.Fatalf("empty parse = %v err=%v", parsed, err)
	}
	for name, raw := range map[string]string{
		"no equals":     "api.anthropic.com",
		"empty host":    "=93.184.216.34",
		"empty address": "api.anthropic.com=",
		"repeated host": "api.anthropic.com=93.184.216.34;api.anthropic.com=93.184.216.35",
		"empty segment": "api.anthropic.com=93.184.216.34;;api.openai.com=160.79.104.10",
	} {
		if _, err := ParsePins(raw); err == nil {
			t.Errorf("%s: ParsePins accepted %q", name, raw)
		}
	}
}

// TestValidateEgressPinsNoopAndRejection proves the model-facing validator is a
// no-op without pins and accepts a public pin for a compiled host.
func TestValidateEgressPinsNoopAndRejection(t *testing.T) {
	if err := ValidateEgressPins(nil, nil); err != nil {
		t.Fatalf("nil pins must be a no-op: %v", err)
	}
	if err := ValidateEgressPins(map[string][]string{}, nil); err != nil {
		t.Fatalf("empty pins must be a no-op: %v", err)
	}
	if err := ValidateEgressPins(map[string][]string{"ark.cn-beijing.volces.com": {"180.184.47.154"}}, nil); err != nil {
		t.Fatalf("public compiled-host pin rejected: %v", err)
	}
}
