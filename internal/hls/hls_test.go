package hls

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func doRequest(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	s.ServeHTTP(rec, req)
	return rec
}

// --- master playlist --------------------------------------------------

func TestMasterPlaylistOnlyListsPresentVariants(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "720p/playlist.m3u8", "#EXTM3U\n#EXT-X-VERSION:3\n")
	writeFile(t, dir, "360p/playlist.m3u8", "#EXTM3U\n#EXT-X-VERSION:3\n")
	// 480p intentionally absent.

	s := New(dir)
	rec := doRequest(t, s, http.MethodGet, "/hls/master.m3u8")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{"720p/playlist.m3u8", "360p/playlist.m3u8"} {
		if !strings.Contains(body, want) {
			t.Errorf("master playlist missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "480p/playlist.m3u8") {
		t.Errorf("master playlist must not list 480p (absent from disk):\n%s", body)
	}
}

func TestMasterPlaylistEmptyReturns503(t *testing.T) {
	s := New(t.TempDir())
	rec := doRequest(t, s, http.MethodGet, "/hls/master.m3u8")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 when no variants exist", rec.Code)
	}
}

// --- media playlist rewriting ----------------------------------------

func TestMediaPlaylistRewritesRelativeSegments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "720p/playlist.m3u8", strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:4",
		"#EXTINF:4.0,",
		"segment_00001.ts",
		"#EXTINF:4.0,",
		"segment_00002.ts",
		"",
	}, "\n"))

	s := New(dir)
	rec := doRequest(t, s, http.MethodGet, "/hls/720p/playlist.m3u8")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"/hls/720p/segment_00001.ts",
		"/hls/720p/segment_00002.ts",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected rewritten segment %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\nsegment_00001.ts\n") {
		t.Errorf("bare segment name was not rewritten:\n%s", body)
	}
}

func TestRewriteSegmentsLeavesAbsoluteURIsAlone(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "https URL", in: "https://cdn.example.com/seg1.ts", want: "https://cdn.example.com/seg1.ts"},
		{name: "absolute path", in: "/absolute/seg2.ts", want: "/absolute/seg2.ts"},
		{name: "comment", in: "#EXT-X-VERSION:3", want: "#EXT-X-VERSION:3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteSegments(tc.in+"\n", "720p")
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in output, got:\n%s", tc.want, got)
			}
		})
	}
}

// --- security: layer 1, variant whitelist ---------------------------

// TestVariantWhitelistRejectsBadFirstComponent exercises paths whose
// FIRST component is not a whitelisted variant. The whitelist alone
// blocks these, regardless of what the second component is.
func TestVariantWhitelistRejectsBadFirstComponent(t *testing.T) {
	s := New(t.TempDir())

	cases := []string{
		"/hls/../etc/passwd",         // ".." as variant
		"/hls/720p/../../etc/passwd", // "720p/../.." collapses to ".."
		"/hls/%2e%2e/etc/passwd",     // encoded ".." in first component
		"/hls/unknown/playlist.m3u8", // unknown name
	}
	for _, p := range cases {
		p := p
		t.Run(p, func(t *testing.T) {
			rec := doRequest(t, s, http.MethodGet, p)
			if rec.Code == http.StatusOK {
				t.Fatalf("whitelist failed to reject %q (status 200)", p)
			}
		})
	}
}

// --- security: layer 2, path separator rejection --------------------

// TestContainsAnyBlocksSlashesInValidVariant exercises paths whose FIRST
// component is a valid variant ("720p") but whose SECOND component
// contains a path separator. The whitelist cannot help here, so the
// ContainsAny check is the sole defense.
func TestContainsAnyBlocksSlashesInValidVariant(t *testing.T) {
	s := New(t.TempDir())

	cases := []string{
		"/hls/720p/../../etc/passwd",
		"/hls/720p/foo/bar.ts",
		"/hls/720p/..%2f..%2fetc%2fpasswd",
		`/hls/720p/..\..\etc\passwd`,
	}
	for _, p := range cases {
		p := p
		t.Run(p, func(t *testing.T) {
			rec := doRequest(t, s, http.MethodGet, p)
			if rec.Code == http.StatusOK {
				t.Fatalf("ContainsAny failed to reject %q (status 200)", p)
			}
		})
	}
}

// TestDotDotInsideFileNameRejected covers the case where ".." appears
// as the entire filename portion of a valid variant path.
func TestDotDotInsideFileNameRejected(t *testing.T) {
	s := New(t.TempDir())
	rec := doRequest(t, s, http.MethodGet, "/hls/720p/..")
	if rec.Code == http.StatusOK {
		t.Fatalf("expected 404 for /hls/720p/.., got 200")
	}
}

// --- segment serving --------------------------------------------------

func TestSegmentServedWithCorrectContentType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "720p/segment_00001.ts", "fake-mpeg-ts-payload")

	s := New(dir)
	rec := doRequest(t, s, http.MethodGet, "/hls/720p/segment_00001.ts")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != "fake-mpeg-ts-payload" {
		t.Errorf("body mismatch: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Content-Type: got %q, want video/mp2t", ct)
	}
}

// --- snapshot ---------------------------------------------------------

func TestSnapshotReportsReadyVariants(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "720p/playlist.m3u8", "#EXTM3U\n")
	writeFile(t, dir, "360p/playlist.m3u8", "#EXTM3U\n")

	snap := New(dir).Snapshot()

	if !snap.Available {
		t.Fatal("expected Available=true")
	}
	if snap.Master != "/hls/master.m3u8" {
		t.Errorf("master: got %q, want /hls/master.m3u8", snap.Master)
	}
	if len(snap.Variants) != 2 {
		t.Fatalf("variants: got %d, want 2", len(snap.Variants))
	}
	seen := map[string]bool{}
	for _, v := range snap.Variants {
		seen[v.Name] = true
	}
	if !seen["720p"] || !seen["360p"] {
		t.Errorf("missing variants in snapshot: %+v", snap.Variants)
	}
	if seen["480p"] {
		t.Errorf("480p should not appear in snapshot (absent from disk)")
	}
}

func TestEmptySnapshot(t *testing.T) {
	snap := New(t.TempDir()).Snapshot()
	if snap.Available {
		t.Fatal("expected Available=false on empty dir")
	}
	if len(snap.Variants) != 0 {
		t.Fatalf("expected 0 variants, got %d", len(snap.Variants))
	}
}

// --- HTTP methods -----------------------------------------------------

func TestMethodNotAllowed(t *testing.T) {
	s := New(t.TempDir())
	rec := doRequest(t, s, http.MethodPost, "/hls/master.m3u8")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestOPTIONSReturns204(t *testing.T) {
	s := New(t.TempDir())
	rec := doRequest(t, s, http.MethodOptions, "/hls/master.m3u8")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
}
