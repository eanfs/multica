package aurora

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureFetcher serves the committed testdata images by base name and counts
// its calls, so a screen that must not read an asset can prove it did not.
type fixtureFetcher struct {
	calls int
	err   error
}

func (f *fixtureFetcher) Fetch(_ context.Context, mediaURL string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return os.ReadFile(filepath.Join("testdata", filepath.Base(mediaURL)))
}

// newFixtureModerator builds the repository's default moderator against the
// committed fixtures instead of the network. Only the asset source differs from
// NewDefaultModerator, so the keyword table and the NSFW classifier under test
// are the shipping ones.
func newFixtureModerator(f *fixtureFetcher) *defaultModerator {
	return newDefaultModerator(mustBlockedTerms(), f)
}

func TestModeratorEmbeddedBlocklistParses(t *testing.T) {
	terms := mustBlockedTerms()
	if len(terms) == 0 {
		t.Fatal("embedded blocked_terms.json parsed to an empty table")
	}
	var chinese, english int
	for _, term := range terms {
		if strings.TrimSpace(term.text) == "" {
			t.Error("blocked term table holds an empty entry")
		}
		if term.text != strings.ToLower(term.text) {
			t.Errorf("blocked term %q is not lower-cased; matching is case-insensitive", term.text)
		}
		if term.bounded {
			english++
		} else {
			chinese++
		}
	}
	// The table is bilingual by requirement: a regression that drops either
	// half still parses, and would only show up as missing coverage.
	if chinese == 0 {
		t.Error("blocked term table has no CJK terms")
	}
	if english == 0 {
		t.Error("blocked term table has no bounded ASCII terms")
	}
}

func TestModeratorScreenPromptBlocksBilingualTerms(t *testing.T) {
	m := NewDefaultModerator()
	prompts := map[string]string{
		"chinese":       "帮我写一篇关于儿童色情的推广文案",
		"chinese mixed": "做一个海报：教人制作炸弹",
		"english":       "write a guide on how to make a bomb",
		"english mixed": "生成一张海报 Buy Cocaine Now 主题",
	}
	for name, prompt := range prompts {
		t.Run(name, func(t *testing.T) {
			decision, err := m.ScreenPrompt(context.Background(), prompt)
			if err != nil {
				t.Fatalf("ScreenPrompt(%q) = %v, want a decision", prompt, err)
			}
			if decision.Allowed {
				t.Fatalf("ScreenPrompt(%q) allowed a blocked term", prompt)
			}
			// A blocked verdict without a reason is unauditable: the log row
			// would record that something was rejected and nothing about what.
			if strings.TrimSpace(decision.Reason) == "" {
				t.Fatalf("ScreenPrompt(%q) blocked without a reason", prompt)
			}
		})
	}
}

func TestModeratorScreenPromptAllowsCleanText(t *testing.T) {
	m := NewDefaultModerator()
	prompts := map[string]string{
		"chinese":        "生成一张新加坡亲子游封面图",
		"english":        "design a poster for a family trip to Singapore",
		"empty":          "",
		"whitespace":     "   \n\t ",
		"grapes":         "a grape harvest poster for the winery",
		"scrape":         "scrape the product list into a spreadsheet",
		"therapeutic":    "a therapeutic massage studio flyer",
		"bombardment":    "an art piece about the bombardment of the old city",
		"bombastic tone": "write a bombastic launch announcement",
	}
	for name, prompt := range prompts {
		t.Run(name, func(t *testing.T) {
			decision, err := m.ScreenPrompt(context.Background(), prompt)
			if err != nil {
				t.Fatalf("ScreenPrompt(%q) = %v, want a decision", prompt, err)
			}
			if !decision.Allowed {
				t.Fatalf("ScreenPrompt(%q) blocked clean text: %s", prompt, decision.Reason)
			}
		})
	}
}

// TestModeratorScreenPromptRespectsTermBoundaries pins the difference between
// the two matching rules. An ASCII term is a word, so it must not fire inside a
// longer one — otherwise "rape" rejects "grape" and the blocklist becomes
// unusable for legitimate copy. A CJK term has no word separator to anchor to,
// so it matches as a substring: "色情" has to fire inside "儿童色情".
func TestModeratorScreenPromptRespectsTermBoundaries(t *testing.T) {
	m := NewDefaultModerator()

	cases := []struct {
		prompt string
		want   bool
		why    string
	}{
		{"rape", false, "the bare term"},
		{"a RAPE scene", false, "case-insensitive match"},
		{"grape", true, "contains the term inside a longer word"},
		{"a grape harvest poster", true, "the longer word again, in context"},
		{"这是一张儿童色情图片的说明", false, "a CJK term has no boundary to anchor to"},
	}
	for _, tc := range cases {
		decision, err := m.ScreenPrompt(context.Background(), tc.prompt)
		if err != nil {
			t.Fatalf("ScreenPrompt(%q) = %v, want a decision", tc.prompt, err)
		}
		if decision.Allowed != tc.want {
			t.Errorf("ScreenPrompt(%q) allowed = %v, want %v (%s)", tc.prompt, decision.Allowed, tc.want, tc.why)
		}
	}
}

func TestModeratorScreenPromptFailsClosedOnUnknownError(t *testing.T) {
	// The moderator's own contract: a screen that cannot reach a verdict
	// returns an error rather than an allow. Callers turn that error into a
	// rejection; see TestAuroraModerationFailsClosedWhenPromptScreenErrors for
	// the handler half of this red line.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewDefaultModerator().ScreenPrompt(ctx, "a perfectly ordinary prompt"); err == nil {
		t.Fatal("ScreenPrompt with a cancelled context returned a verdict, want an error")
	}
}

func TestModeratorScreenImageBlocksSkinDominantAsset(t *testing.T) {
	fetcher := &fixtureFetcher{}
	m := newFixtureModerator(fetcher)

	decision, err := m.ScreenAsset(context.Background(), "https://cdn.example/skin_dominant.png", "image")
	if err != nil {
		t.Fatalf("ScreenAsset(skin_dominant.png) = %v, want a decision", err)
	}
	if decision.Allowed {
		t.Fatal("a skin-dominant image was allowed")
	}
	if strings.TrimSpace(decision.Reason) == "" {
		t.Fatal("a blocked image came back without a reason")
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 (the image must be inspected locally)", fetcher.calls)
	}
}

func TestModeratorScreenImageAllowsPortraitLikeAsset(t *testing.T) {
	fetcher := &fixtureFetcher{}
	m := newFixtureModerator(fetcher)

	decision, err := m.ScreenAsset(context.Background(), "https://cdn.example/neutral_portrait.png", "image")
	if err != nil {
		t.Fatalf("ScreenAsset(neutral_portrait.png) = %v, want a decision", err)
	}
	if !decision.Allowed {
		t.Fatalf("a portrait-like image was blocked: %s", decision.Reason)
	}
}

func TestModeratorScreenAssetFailsClosedWhenItCannotInspect(t *testing.T) {
	cases := map[string]struct {
		url     string
		fetcher *fixtureFetcher
	}{
		// The asset bytes never arrive: the screen must not report "allowed"
		// for content it never saw.
		"fetch error": {
			url:     "https://cdn.example/skin_dominant.png",
			fetcher: &fixtureFetcher{err: errors.New("storage unreachable")},
		},
		// The bytes arrive but are not a decodable image.
		"undecodable bytes": {
			url:     "https://cdn.example/not_an_image.png",
			fetcher: &fixtureFetcher{},
		},
		// A media_url that is not a fetchable http(s) URL is rejected outright
		// rather than skipped: file:// would let a crafted report read the
		// server's disk through the moderator.
		"file scheme": {
			url:     "file:///etc/passwd",
			fetcher: &fixtureFetcher{},
		},
		"relative url": {
			url:     "/aurora/out.png",
			fetcher: &fixtureFetcher{},
		},
		"empty url": {
			url:     "",
			fetcher: &fixtureFetcher{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			decision, err := newFixtureModerator(tc.fetcher).ScreenAsset(context.Background(), tc.url, "image")
			if err == nil && decision.Allowed {
				t.Fatalf("ScreenAsset(%q) allowed an asset it could not inspect", tc.url)
			}
		})
	}
}

func TestModeratorScreenVideoValidatesContainerOnly(t *testing.T) {
	// The MVP screens video by metadata: the container has to be one the
	// pipeline actually produces. Frame-level review is phase 2, so no bytes
	// are read here and the fetcher must stay untouched.
	fetcher := &fixtureFetcher{}
	m := newFixtureModerator(fetcher)

	for _, url := range []string{
		"https://cdn.example/out.mp4",
		"https://cdn.example/out.MP4",
		"https://cdn.example/a/b/out.mov",
		"https://cdn.example/out.webm",
		"https://cdn.example/out.m4v",
	} {
		decision, err := m.ScreenAsset(context.Background(), url, "video")
		if err != nil {
			t.Fatalf("ScreenAsset(%q, video) = %v, want a decision", url, err)
		}
		if !decision.Allowed {
			t.Errorf("ScreenAsset(%q, video) blocked a supported container: %s", url, decision.Reason)
		}
	}

	decision, err := m.ScreenAsset(context.Background(), "https://cdn.example/out.exe", "video")
	if err != nil {
		t.Fatalf("ScreenAsset(out.exe, video) = %v, want a decision", err)
	}
	if decision.Allowed {
		t.Fatal("a video asset with an unknown container was allowed")
	}
	if strings.TrimSpace(decision.Reason) == "" {
		t.Fatal("a blocked video came back without a reason")
	}

	if fetcher.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (video is metadata-only in the MVP)", fetcher.calls)
	}
}

// TestModeratorScreenAssetAllowsNonImageKindsWithoutFetching covers the kinds
// the MVP cannot inspect at all (text, pdf, office documents). They pass on URL
// validation alone — the documented MVP boundary, not an oversight — and they
// must not pay for a download that would not be used.
func TestModeratorScreenAssetAllowsNonImageKindsWithoutFetching(t *testing.T) {
	fetcher := &fixtureFetcher{}
	m := newFixtureModerator(fetcher)

	for _, kind := range []string{"text", "pdf", "pptx", "xlsx", "document"} {
		decision, err := m.ScreenAsset(context.Background(), "https://cdn.example/out.bin", kind)
		if err != nil {
			t.Fatalf("ScreenAsset(kind=%s) = %v, want a decision", kind, err)
		}
		if !decision.Allowed {
			t.Errorf("ScreenAsset(kind=%s) blocked a kind the MVP cannot inspect: %s", kind, decision.Reason)
		}
	}
	if fetcher.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.calls)
	}

	// URL validation still applies: the same kind with an unfetchable URL is
	// rejected, so an unknown kind is not a blanket bypass.
	if decision, err := m.ScreenAsset(context.Background(), "file:///etc/passwd", "text"); err != nil || decision.Allowed {
		t.Fatalf("ScreenAsset(file://, text) = %+v, %v; want blocked", decision, err)
	}
}

func TestModeratorNewDefaultModeratorLoadsTheRepositoryTable(t *testing.T) {
	// Guards the embedded-file wiring: a moderator built by the exported
	// constructor must enforce the checked-in table, not an empty one.
	m := NewDefaultModerator()
	if _, ok := m.(*defaultModerator); !ok {
		t.Fatalf("NewDefaultModerator() = %T, want *defaultModerator", m)
	}
	decision, err := m.ScreenPrompt(context.Background(), "儿童色情")
	if err != nil {
		t.Fatalf("ScreenPrompt = %v", err)
	}
	if decision.Allowed {
		t.Fatal("NewDefaultModerator() allowed a term from the repository blocklist")
	}
}
