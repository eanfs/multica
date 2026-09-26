package aurora

// Provider artifact import fetch (Plan C Task 6).
//
// The URL a provider returns is attacker-influenced from this server's point of
// view: the sandbox broker forwards whatever the provider returned, so a
// compromised or malicious provider response could name any host. The fetch is
// guarded the same way the moderation asset fetch in moderation.go is — the
// decision is made on the address actually dialled, never on the hostname —
// extended with the two things an import needs and a screening fetch does not:
// a bounded redirect chain, revalidated on every hop and kept on HTTPS, and an
// address policy a test can substitute for the loopback its fake server lives
// on.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// ErrArtifactAddrBlocked is returned instead of a connection when a provider
// result host resolves to an address the deployment must not be pointed at. It
// is deliberately distinct from a dial failure so the handler can answer 400
// rather than 502: only this error means the caller supplied a URL it should
// not have.
var ErrArtifactAddrBlocked = errors.New("aurora: artifact source resolves to a non-public address")

// ErrArtifactRedirectRefused is returned when a provider result redirect is
// refused: a chain longer than five hops, or a hop that leaves HTTPS.
var ErrArtifactRedirectRefused = errors.New("aurora: artifact source redirect refused")

// ArtifactHostResolver is net.Resolver's one method the import dialer needs, so
// a test can answer for a name without touching the network.
type ArtifactHostResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ArtifactAddrPolicy reports whether one resolved address may be dialled. A nil
// policy means the production one, IsPublicAddress.
type ArtifactAddrPolicy func(netip.Addr) bool

// IsPublicAddress reports whether the server may connect to addr: anything that
// is not routable public internet is refused, including the carrier-grade NAT
// and documentation ranges the standard library does not classify as private.
// It is the exported face of isPublicIP so the handler and its tests share one
// public-address definition.
func IsPublicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	return isPublicIP(net.IP(addr.AsSlice()))
}

const (
	artifactImportDialTimeout  = 10 * time.Second
	artifactImportMaxRedirects = 5
)

// NewArtifactImportClient builds the HTTP client every provider artifact import
// goes through. resolver and allow are test seams; nil selects
// net.DefaultResolver and IsPublicAddress.
func NewArtifactImportClient(resolver ArtifactHostResolver, allow ArtifactAddrPolicy) *http.Client {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if allow == nil {
		allow = IsPublicAddress
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// An HTTP proxy would carry the request to a host the dial guard never
	// sees, which is the whole point of the guard, so HTTP_PROXY is ignored.
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: artifactImportDialTimeout}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			return nil, fmt.Errorf("aurora: resolve artifact source %q: %w", host, err)
		}
		// Every answer has to be allowed, not just the one about to be dialled:
		// a host that answers with both a public and an internal address would
		// otherwise slip through on whichever answer the resolver ordered first.
		for _, resolved := range addresses {
			if !allow(resolved) {
				return nil, fmt.Errorf("%w: %s", ErrArtifactAddrBlocked, resolved)
			}
		}
		// Dial the literal that passed the check, never re-resolve: resolving
		// twice is the DNS-rebinding window.
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= artifactImportMaxRedirects {
				return fmt.Errorf("%w: more than %d redirects", ErrArtifactRedirectRefused, artifactImportMaxRedirects)
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect left HTTPS", ErrArtifactRedirectRefused)
			}
			// The importer sends no credentials, and a redirect must not
			// acquire any.
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}
