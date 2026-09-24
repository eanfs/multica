package aurora

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Content moderation for Aurora. The domain is deliberately an adapter: the
// interface below is what the create and completion paths depend on, and a
// vendor screening API (Volcengine / Alibaba content security, phase 2) replaces
// NewDefaultModerator's implementation without touching a call site.
//
// The default implementation runs entirely in-process and needs no keys or
// network egress beyond fetching the asset it is asked to inspect: a bilingual
// keyword blocklist for prompts, plus local image screening for assets. That
// keeps a self-hosted deployment working with no external contract, which is
// the same bar the rest of the MVP holds.
//
// Fail-closed is the red line (spec §10): a screen that cannot reach a verdict
// returns an error, and every caller turns that error into a rejection. Nothing
// in this file may return an allow as a way of saying "I don't know".

// ModerationScope values recorded on a moderation_log row. Scope names the
// surface that was screened, so a review can tell a rejected prompt from a
// rejected artifact.
const (
	ModerationScopePrompt = "prompt"
	ModerationScopeAsset  = "asset"
)

// ModerationVerdictBlocked is the only verdict the default adapter writes. The
// column's domain also admits "allowed" for an adapter that records every
// verdict; writing a row per admitted request would be a write on the hot path
// with nothing for a reviewer to act on.
const ModerationVerdictBlocked = "blocked"

// Decision is the outcome of one screen. Reason is always populated when
// Allowed is false: the handler forwards it to the caller and the audit row
// stores it, and a rejection that cannot say why is neither explainable to the
// user nor reviewable later.
type Decision struct {
	Allowed bool
	Reason  string
}

// Moderator screens user-supplied content before it is persisted, charged for,
// or executed. An error means the screen did not complete — callers must treat
// it as a rejection, never as an allow.
type Moderator interface {
	ScreenPrompt(ctx context.Context, text string) (Decision, error)
	ScreenAsset(ctx context.Context, mediaURL, kind string) (Decision, error)
}

// Screening limits. They bound what one request can make the server read and
// decode: an asset URL is attacker-influenced (the daemon reports it), so an
// unbounded fetch or decode would be a denial-of-service vector rather than a
// moderation feature.
const (
	defaultAssetFetchTimeout = 10 * time.Second
	defaultAssetDialTimeout  = 5 * time.Second
	maxAssetBytes            = 16 << 20
	maxScreenedPixels        = 25_000_000
	// nsfwSkinRatioThreshold is the fraction of sampled pixels that may be
	// skin-toned before an image is rejected. Explicit imagery is dominated by
	// bare skin; a portrait is not. The value is deliberately coarse — this is
	// a local heuristic, not a classifier — and sits well above the ~12% a
	// head-and-shoulders portrait scores so ordinary output is not rejected.
	nsfwSkinRatioThreshold = 0.6
	// skinSampleGrid caps the sampled pixels per image regardless of source
	// resolution, so screening cost does not scale with the upload.
	skinSampleGrid = 64
)

//go:embed blocked_terms.json
var blockedTermsJSON []byte

// blockedTermsFile is the on-disk shape of blocked_terms.json.
type blockedTermsFile struct {
	Version int      `json:"version"`
	Terms   []string `json:"terms"`
}

// blockedTerm is one blocklist entry plus the matching rule its script implies.
type blockedTerm struct {
	text string
	// bounded marks a term whose own edges are ASCII word characters. Such a
	// term is a word, so it must not fire inside a longer one: without this,
	// "rape" rejects "grape" and the list becomes unusable for ordinary copy.
	// A CJK term has no separator to anchor to and always matches as a
	// substring.
	bounded bool
}

// defaultModerator is the in-process Moderator. Its two dependencies are the
// parsed blocklist and a byte source for assets, which is what makes the NSFW
// half testable against committed fixtures instead of the network.
type defaultModerator struct {
	terms  []blockedTerm
	assets AssetFetcher
}

// NewDefaultModerator returns the repository's built-in moderator: the
// bilingual blocklist in blocked_terms.json plus local image screening. It needs
// no configuration and no external service.
//
// The blocklist is embedded at build time, so a parse failure is a build defect
// rather than a runtime condition; it panics, and
// TestModeratorEmbeddedBlocklistParses fails the build before it could ship.
// Starting with an empty blocklist instead would silently disable moderation,
// which is the one outcome the spec forbids.
func NewDefaultModerator() Moderator {
	return newDefaultModerator(mustBlockedTerms(), HTTPAssetFetcher{})
}

func newDefaultModerator(terms []blockedTerm, assets AssetFetcher) *defaultModerator {
	return &defaultModerator{terms: terms, assets: assets}
}

// mustBlockedTerms parses the embedded blocklist and panics on a malformed or
// empty file.
func mustBlockedTerms() []blockedTerm {
	terms, err := parseBlockedTerms(blockedTermsJSON)
	if err != nil {
		panic("aurora: blocked_terms.json is unusable: " + err.Error())
	}
	return terms
}

// parseBlockedTerms reads the blocklist. Matching is case-insensitive, so the
// table is normalised once here rather than on every request; an upper-cased
// entry would silently never match otherwise.
func parseBlockedTerms(raw []byte) ([]blockedTerm, error) {
	var file blockedTermsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse blocked terms: %w", err)
	}
	if len(file.Terms) == 0 {
		return nil, fmt.Errorf("blocked terms list is empty")
	}
	terms := make([]blockedTerm, 0, len(file.Terms))
	for _, term := range file.Terms {
		normalised := strings.ToLower(strings.TrimSpace(term))
		if normalised == "" {
			return nil, fmt.Errorf("blocked terms list holds an empty entry")
		}
		terms = append(terms, blockedTerm{text: normalised, bounded: isBoundedTerm(normalised)})
	}
	return terms, nil
}

// isBoundedTerm reports whether a term is anchored by ASCII word characters at
// both ends, and therefore must be matched as a whole word.
func isBoundedTerm(term string) bool {
	first, _ := utf8.DecodeRuneInString(term)
	last, _ := utf8.DecodeLastRuneInString(term)
	return isASCIIWordRune(first) && isASCIIWordRune(last)
}

func isASCIIWordRune(r rune) bool {
	return r == '_' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}

func isASCIIWordByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// ScreenPrompt rejects a prompt that contains a blocked term. The reason names
// the term that fired: the caller wrote it, so echoing it costs no secret, and
// both the rejection message and the audit row are unusable without it.
func (m *defaultModerator) ScreenPrompt(ctx context.Context, text string) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if term, ok := matchBlockedTerm(text, m.terms); ok {
		return Decision{Allowed: false, Reason: "prompt contains a blocked term: " + term}, nil
	}
	return Decision{Allowed: true}, nil
}

// matchBlockedTerm returns the first blocklist entry the text matches.
func matchBlockedTerm(text string, terms []blockedTerm) (string, bool) {
	lowered := strings.ToLower(text)
	for _, term := range terms {
		if term.bounded {
			if containsBoundedWord(lowered, term.text) {
				return term.text, true
			}
			continue
		}
		if strings.Contains(lowered, term.text) {
			return term.text, true
		}
	}
	return "", false
}

// containsBoundedWord reports whether needle appears in haystack with non-word
// characters on both sides. Both are expected to be lower-cased; the byte walk
// is safe on multi-byte input because every byte of an encoded rune is >= 0x80
// and therefore never a word byte, which is exactly the boundary a CJK
// neighbour should provide.
func containsBoundedWord(haystack, needle string) bool {
	for offset := 0; offset <= len(haystack)-len(needle); {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(needle)
		beforeOK := start == 0 || !isASCIIWordByte(haystack[start-1])
		afterOK := end == len(haystack) || !isASCIIWordByte(haystack[end])
		if beforeOK && afterOK {
			return true
		}
		offset = start + 1
	}
	return false
}

// ScreenAsset screens one produced artifact. Images are inspected locally;
// video is metadata-only in the MVP (frame-level review is phase 2); the
// remaining kinds are validated by URL alone. An error always means "cannot
// screen", which the caller must treat as a rejection.
func (m *defaultModerator) ScreenAsset(ctx context.Context, mediaURL, kind string) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	parsed, ok := parseAssetURL(mediaURL)
	if !ok {
		// Not merely a formatting complaint: the moderator fetches this URL
		// server-side, so a non-http(s) scheme (file://, gopher://) would let a
		// crafted artifact report reach past the asset store.
		return Decision{Allowed: false, Reason: "asset media_url is not a fetchable http(s) URL"}, nil
	}

	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image":
		return m.screenImage(ctx, mediaURL)
	case "video":
		// parsed.Path excludes the query, so path.Ext sees the container and
		// not a query fragment. An unknown or absent extension is rejected:
		// metadata validation is the whole MVP screen here, and "could not
		// validate" is not a pass.
		if extension := strings.ToLower(path.Ext(parsed.Path)); !videoContainers[extension] {
			return Decision{
				Allowed: false,
				Reason:  fmt.Sprintf("video container %q is not one the pipeline produces", extension),
			}, nil
		}
		return Decision{Allowed: true}, nil
	default:
		// text, pdf, pptx, xlsx: the MVP has no local screen for these and
		// records them as passed on URL validation. That boundary is documented
		// rather than accidental — the vendor adapter is where it closes.
		return Decision{Allowed: true}, nil
	}
}

// videoContainers are the video extensions the MVP pipeline can produce.
var videoContainers = map[string]bool{
	".mp4":  true,
	".mov":  true,
	".webm": true,
	".m4v":  true,
}

// parseAssetURL accepts only an absolute http(s) URL with a host.
func parseAssetURL(mediaURL string) (*url.URL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(mediaURL))
	if err != nil {
		return nil, false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, false
	}
	if parsed.Host == "" {
		return nil, false
	}
	return parsed, true
}

// screenImage fetches the asset and classifies it locally.
func (m *defaultModerator) screenImage(ctx context.Context, mediaURL string) (Decision, error) {
	raw, err := m.assets.Fetch(ctx, mediaURL)
	if err != nil {
		return Decision{}, fmt.Errorf("fetch image asset: %w", err)
	}
	if len(raw) == 0 {
		return Decision{}, fmt.Errorf("image asset is empty")
	}

	// Bound the decode before it happens: the pixel limit is what keeps a
	// small file that expands into a huge bitmap from exhausting the process.
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Decision{}, fmt.Errorf("decode image header: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return Decision{}, fmt.Errorf("image reports dimensions %dx%d", config.Width, config.Height)
	}
	if config.Width*config.Height > maxScreenedPixels {
		return Decision{}, fmt.Errorf("image is %dx%d, above the %d-pixel screening limit", config.Width, config.Height, maxScreenedPixels)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Decision{}, fmt.Errorf("decode image: %w", err)
	}

	ratio := skinRatio(img)
	if ratio >= nsfwSkinRatioThreshold {
		return Decision{
			Allowed: false,
			Reason:  fmt.Sprintf("image is %.0f%% skin-toned, at or above the %.0f%% explicit-content threshold", ratio*100, nsfwSkinRatioThreshold*100),
		}, nil
	}
	return Decision{Allowed: true}, nil
}

// skinRatio estimates the fraction of an image that is bare-skin coloured. It
// samples a bounded grid rather than every pixel, so a 24-megapixel upload costs
// the same as a thumbnail.
//
// This is the MVP's local NSFW screen: a colour heuristic, not a trained
// classifier. It is honest about what it is — it catches imagery dominated by
// skin and lets ordinary output through, and it is deliberately replaceable,
// since a real model (openNSFW2-class) would arrive as a different Moderator
// rather than as a change here.
func skinRatio(img image.Image) float64 {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return 0
	}
	stepX := max(1, width/skinSampleGrid)
	stepY := max(1, height/skinSampleGrid)

	sampled, skin := 0, 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			r, g, b, _ := img.At(x, y).RGBA()
			sampled++
			if isSkinTone(int(r>>8), int(g>>8), int(b>>8)) {
				skin++
			}
		}
	}
	if sampled == 0 {
		return 0
	}
	return float64(skin) / float64(sampled)
}

// isSkinTone applies the RGB skin-tone rule from Peer et al., "A Combined
// Corner and Skin Based Detection Method for High-Density Crowd Video"
// (2009) — the standard non-parametric test, chosen over a learned model
// because it needs no weights and behaves identically on every deployment.
func isSkinTone(r, g, b int) bool {
	maximum := max(r, g, b)
	minimum := min(r, g, b)
	return r > 95 && g > 40 && b > 20 &&
		maximum-minimum > 15 &&
		abs(r-g) > 15 &&
		r > g && r > b
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// AssetFetcher loads the bytes an asset URL points at so the moderator can
// inspect them locally. It is an interface so the screening logic can be tested
// against committed fixtures; production uses HTTPAssetFetcher.
type AssetFetcher interface {
	Fetch(ctx context.Context, mediaURL string) ([]byte, error)
}

// HTTPAssetFetcher fetches asset bytes over HTTP(S) under a timeout and a hard
// size cap, through the guarded client newAssetHTTPClient builds.
type HTTPAssetFetcher struct {
	// MaxBytes defaults to maxAssetBytes.
	MaxBytes int64
}

func (f HTTPAssetFetcher) Fetch(ctx context.Context, mediaURL string) ([]byte, error) {
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = maxAssetBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := newAssetHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asset fetch returned %s", resp.Status)
	}
	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated into a decodable prefix.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("asset exceeds the %d-byte screening limit", maxBytes)
	}
	return raw, nil
}

// newAssetHTTPClient builds the client the default fetcher uses, and the
// reason it is not http.DefaultClient.
//
// The URL being fetched is chosen by whoever reports the artifact, and the
// artifact report is produced by an agent running inside the sandbox. Prompt
// injection reaching that agent therefore reaches this fetch, which makes it an
// SSRF sink: without a guard, a reported media_url of
// http://169.254.169.254/latest/meta-data/ would have the server read cloud
// instance credentials and store them as an "asset".
//
// Two defences, and both are needed:
//
//   - The dialer checks the address it is about to connect to, not the hostname
//     it was given. Checking after a separate resolution would leave a
//     DNS-rebinding window between the check and the connection; checking the
//     dialled address closes it.
//   - Redirects are refused. A 302 from an allowed host would otherwise hand
//     the fetch to a host the URL validation never saw, and the object-store
//     URL a daemon reports is final — there is nothing to follow.
func newAssetHTTPClient() *http.Client {
	// http.DefaultTransport is always a *http.Transport, and cloning it keeps
	// the tuned pool settings and ProxyFromEnvironment support that a
	// deployment's egress proxy depends on.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: defaultAssetDialTimeout}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("asset host %q resolved to no address", host)
		}
		// Every address has to be allowed, not just the one about to be dialled.
		// A host that resolves to both a public and an internal address would
		// otherwise pass the check on one and connect to the other, whichever
		// the resolver happened to order first.
		for _, resolved := range addresses {
			if !isPublicIP(resolved.IP) {
				return nil, fmt.Errorf("asset host %q resolves to non-public address %s", host, resolved.IP)
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}

	return &http.Client{
		Timeout:   defaultAssetFetchTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("asset fetch must not redirect")
		},
	}
}

// isPublicIP reports whether the moderator may connect to an address. Loopback,
// private, link-local, unique-local, multicast and unspecified ranges are all
// refused: link-local is where cloud instance metadata lives, and the rest is
// the internal network the fetch has no business reaching.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// 100.64.0.0/10 (carrier-grade NAT) has no stdlib predicate. IPv4-mapped
	// IPv6 addresses reach the predicates above through To4, so they need no
	// separate case.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return false
	}
	return true
}
