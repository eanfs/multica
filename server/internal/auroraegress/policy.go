// Package auroraegress enforces Aurora sandbox outbound access through a
// narrow HTTP/CONNECT proxy. Only the exact Multica server origin and the
// compiled provider hosts are reachable, and provider targets must resolve
// exclusively to public addresses.
package auroraegress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CompiledProviderHosts are the provider endpoints every workspace may reach.
// The list is compiled into the binary; operators may add further exact
// host:443 entries but never wildcards or arbitrary ports.
var CompiledProviderHosts = []string{
	"api.anthropic.com:443",
	"ark.cn-beijing.volces.com:443",
	"api.openai.com:443",
	"openspeech.bytedance.com:443",
}

// ErrTargetRefused marks every authorization refusal so callers can classify a
// denied target without parsing messages.
var ErrTargetRefused = errors.New("egress target refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTargetRefused, fmt.Sprintf(format, args...))
}

// ResolveFunc resolves a host to its A/AAAA answers. Tests inject fakes so the
// authorization matrix runs offline.
type ResolveFunc func(context.Context, string) ([]net.IPAddr, error)

// CompiledProviderTLSHosts returns the compiled allowlist in a stable order.
func CompiledProviderTLSHosts() []string {
	hosts := make([]string, len(CompiledProviderHosts))
	copy(hosts, CompiledProviderHosts)
	return hosts
}

// Policy is the egress authorization policy. The zero value denies everything:
// an unconfigured sidecar must never forward traffic.
type Policy struct {
	// ServerOrigin is the exact Multica origin sandboxes call back to. Its
	// address may resolve privately; only its scheme, host and port are
	// excepted and nothing about the exception generalizes.
	ServerOrigin *url.URL
	// AllowedTLSHosts holds the exact host:port keys usable as CONNECT targets.
	AllowedTLSHosts map[string]struct{}
	// Resolve returns the A/AAAA answers for a host. It defaults to the system
	// resolver and is replaced wholesale in tests.
	Resolve ResolveFunc
}

// Target is an authorized destination. Host is the host:port the caller dials
// and Addrs are the pinned addresses a provider target must be dialed on; the
// server-origin exception pins nothing because it may be reached by name.
type Target struct {
	Host     string
	Addrs    []net.IPAddr
	Server   bool
	Upstream *url.URL
}

// NewPolicy builds a policy for the configured server origin plus optional
// extra exact host:443 entries. It fails closed on malformed or wildcard input.
func NewPolicy(serverOrigin string, extraAllowed []string) (Policy, error) {
	origin, err := url.Parse(strings.TrimSpace(serverOrigin))
	if err != nil {
		return Policy{}, fmt.Errorf("invalid server origin %q: %w", serverOrigin, err)
	}
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return Policy{}, fmt.Errorf("server origin %q must use http or https", serverOrigin)
	}
	if origin.Hostname() == "" {
		return Policy{}, fmt.Errorf("server origin %q has no host", serverOrigin)
	}
	if origin.User != nil {
		return Policy{}, fmt.Errorf("server origin %q must not carry credentials", serverOrigin)
	}
	if (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" {
		return Policy{}, fmt.Errorf("server origin %q must be an origin without a path, query or fragment", serverOrigin)
	}

	allowed := make(map[string]struct{}, len(CompiledProviderHosts)+len(extraAllowed))
	for _, host := range CompiledProviderHosts {
		allowed[host] = struct{}{}
	}
	for _, raw := range extraAllowed {
		host, err := validateEgressHost(raw)
		if err != nil {
			return Policy{}, err
		}
		allowed[host] = struct{}{}
	}

	return Policy{
		ServerOrigin:    origin,
		AllowedTLSHosts: allowed,
		Resolve:         net.DefaultResolver.LookupIPAddr,
	}, nil
}

// validateEgressHost accepts only an exact host name bound to port 443.
func validateEgressHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return "", fmt.Errorf("egress host %q must not be empty", raw)
	}
	if strings.Contains(host, "*") {
		return "", fmt.Errorf("egress host %q must not contain a wildcard", raw)
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return "", fmt.Errorf("egress host %q must be an exact host:443", raw)
	}
	if name == "" || port != "443" {
		return "", fmt.Errorf("egress host %q must be an exact host:443", raw)
	}
	if net.ParseIP(name) != nil {
		return "", fmt.Errorf("egress host %q must be a name, not an IP literal", raw)
	}
	return host, nil
}

// Authorize reports whether the target may be reached through the proxy.
// isConnect is true for CONNECT (tunnelled TLS) requests.
func (p Policy) Authorize(target *url.URL, isConnect bool) error {
	_, err := p.Validate(context.Background(), target, isConnect)
	return err
}

// Validate authorizes the target and returns the pinned destination. Provider
// targets are resolved exactly once here, so a later rebinding answer cannot
// redirect an established tunnel; the returned addresses are the only ones the
// caller may dial.
func (p Policy) Validate(ctx context.Context, target *url.URL, isConnect bool) (Target, error) {
	if target == nil {
		return Target{}, refuse("nil target")
	}
	if target.User != nil {
		return Target{}, refuse("credentials in the target are not allowed")
	}
	host := strings.ToLower(target.Hostname())
	if host == "" {
		return Target{}, refuse("target has no host")
	}

	// The server-origin exception is exact: same scheme, host and port. It is
	// never generalized to a sibling port or another host sharing an address,
	// and it is the only target allowed to resolve privately.
	if p.isServerOrigin(target, host) {
		addrs, err := p.resolve(ctx, host)
		if err != nil {
			addrs = nil
		}
		return Target{Host: target.Host, Addrs: addrs, Server: true, Upstream: target}, nil
	}

	if !isConnect {
		return Target{}, refuse("plain HTTP is only allowed to the configured server origin")
	}
	if target.Scheme != "https" {
		return Target{}, refuse("provider targets must use https, got %q", target.Scheme)
	}
	if net.ParseIP(host) != nil {
		return Target{}, refuse("IP literal targets are not allowed")
	}
	port := target.Port()
	if port != "443" {
		return Target{}, refuse("provider targets must use an explicit port 443")
	}
	key := net.JoinHostPort(host, port)
	if _, ok := p.AllowedTLSHosts[key]; !ok {
		return Target{}, refuse("host %s is not allowed", key)
	}
	if p.Resolve == nil {
		return Target{}, refuse("no resolver configured for %s", key)
	}

	addrs, err := p.Resolve(ctx, host)
	if err != nil {
		return Target{}, refuse("resolve %s: %v", host, err)
	}
	if len(addrs) == 0 {
		return Target{}, refuse("resolve %s: no addresses", host)
	}
	for _, addr := range addrs {
		if !IsPublicIP(addr.IP) {
			return Target{}, refuse("host %s resolves to non-public address %s", host, addr.IP)
		}
	}
	return Target{Host: key, Addrs: addrs, Upstream: target}, nil
}

// resolve is a best-effort lookup: the server origin is dialed by name when it
// cannot be resolved locally, so a lookup failure is not fatal there.
func (p Policy) resolve(ctx context.Context, host string) ([]net.IPAddr, error) {
	if p.Resolve == nil {
		return nil, errors.New("no resolver configured")
	}
	return p.Resolve(ctx, host)
}

// isServerOrigin reports whether the target is exactly the configured origin.
func (p Policy) isServerOrigin(target *url.URL, host string) bool {
	if p.ServerOrigin == nil {
		return false
	}
	if !strings.EqualFold(target.Scheme, p.ServerOrigin.Scheme) {
		return false
	}
	if host != strings.ToLower(p.ServerOrigin.Hostname()) {
		return false
	}
	return effectivePort(target) == effectivePort(p.ServerOrigin)
}

func effectivePort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// IsPublicIP reports whether ip is routable on the public internet. Loopback,
// RFC1918, link-local, CGNAT, multicast, documentation, benchmark, unspecified
// and cloud metadata ranges are all rejected.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		for _, block := range blockedIPv4 {
			if block.Contains(v4) {
				return false
			}
		}
		return true
	}
	for _, block := range blockedIPv6 {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

func mustCIDR(cidr string) *net.IPNet {
	_, block, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return block
}

var blockedIPv4 = []*net.IPNet{
	mustCIDR("0.0.0.0/8"),       // unspecified and "this network"
	mustCIDR("10.0.0.0/8"),      // RFC1918
	mustCIDR("100.64.0.0/10"),   // CGNAT
	mustCIDR("127.0.0.0/8"),     // loopback
	mustCIDR("169.254.0.0/16"),  // link-local, includes 169.254.169.254 metadata
	mustCIDR("172.16.0.0/12"),   // RFC1918
	mustCIDR("192.0.0.0/24"),    // IETF protocol assignments
	mustCIDR("192.0.2.0/24"),    // documentation
	mustCIDR("192.88.99.0/24"),  // 6to4 relay anycast
	mustCIDR("192.168.0.0/16"),  // RFC1918
	mustCIDR("198.18.0.0/15"),   // benchmarking
	mustCIDR("198.51.100.0/24"), // documentation
	mustCIDR("203.0.113.0/24"),  // documentation
	mustCIDR("224.0.0.0/4"),     // multicast
	mustCIDR("240.0.0.0/4"),     // reserved, includes broadcast
}

var blockedIPv6 = []*net.IPNet{
	mustCIDR("::/128"),        // unspecified
	mustCIDR("::1/128"),       // loopback
	mustCIDR("64:ff9b::/96"),  // NAT64
	mustCIDR("100::/64"),      // discard-only
	mustCIDR("2001::/32"),     // Teredo
	mustCIDR("2001:2::/48"),   // benchmarking
	mustCIDR("2001:db8::/32"), // documentation
	mustCIDR("2001:10::/28"),  // ORCHID
	mustCIDR("2002::/16"),     // 6to4
	mustCIDR("fc00::/7"),      // unique local
	mustCIDR("fe80::/10"),     // link-local
	mustCIDR("ff00::/8"),      // multicast
}
