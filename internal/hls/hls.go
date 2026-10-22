// Package hls serves HLS playlists and segments for the fallback path.
package hls

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Variant struct {
	Name      string `json:"name"`
	Bandwidth int    `json:"bandwidth"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Codecs    string `json:"-"`
}

var DefaultVariants = []Variant{
	{Name: "720p", Bandwidth: 2200000, Width: 1280, Height: 720, Codecs: "avc1.42e01e"},
	{Name: "480p", Bandwidth: 900000, Width: 854, Height: 480, Codecs: "avc1.42e01e"},
	{Name: "360p", Bandwidth: 450000, Width: 640, Height: 360, Codecs: "avc1.42e01e"},
}

type Server struct {
	Dir      string
	Variants []Variant
}

func New(dir string) *Server { return &Server{Dir: dir, Variants: DefaultVariants} }

// isValidVariant is the sole authority for the first path component.
// Anything not in the configured variant list is rejected, which is what
// prevents path traversal through crafted variant names ("..", "etc", …).
func (s *Server) isValidVariant(name string) bool {
	for _, v := range s.Variants {
		if v.Name == name {
			return true
		}
	}
	return false
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel := strings.TrimPrefix(r.URL.Path, "/hls/")
	if rel == "" || rel == "master.m3u8" {
		s.serveMaster(w)
		return
	}

	// Security model (rel is already URL-decoded by net/http):
	//
	//   1. The first path component MUST be a whitelisted variant name.
	//      This rejects "../x", "etc", "..", absolute paths, etc.
	//   2. The second component MUST NOT contain "/" or "\".
	//      This rejects anything that would escape <Dir>/<variant>/.
	//
	// Together, those two rules guarantee that filepath.Join(s.Dir,
	// variant, file) resolves inside a known variant directory. We do
	// NOT call path.Clean here: on an absolute path it can never produce
	// a leading "/..", so a naive Clean-based check would be dead code.
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	variant, file := parts[0], parts[1]

	if !s.isValidVariant(variant) {
		http.NotFound(w, r)
		return
	}
	if file == "" || file == "." || file == ".." ||
		strings.ContainsAny(file, `/\`) {
		http.NotFound(w, r)
		return
	}

	switch path.Ext(file) {
	case ".m3u8":
		s.serveMediaPlaylist(w, r, variant, file)
	case ".ts", ".m4s", ".mp4":
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "public, max-age=60")
		http.ServeFile(w, r, filepath.Join(s.Dir, variant, file))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveMaster(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	found := 0
	for _, v := range s.Variants {
		if _, err := os.Stat(filepath.Join(s.Dir, v.Name, "playlist.m3u8")); err != nil {
			continue
		}
		found++
		fmt.Fprintf(&b,
			"#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=%q\n%s/playlist.m3u8\n",
			v.Bandwidth, v.Width, v.Height, v.Codecs, v.Name)
	}
	if found == 0 {
		// A valid-but-empty playlist would make hls.js retry with the
		// same result. A 503 tells it to back off, which is the right
		// behavior while GStreamer is still warming up.
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	fmt.Fprint(w, b.String())
}

func (s *Server) serveMediaPlaylist(w http.ResponseWriter, r *http.Request, variant, file string) {
	data, err := os.ReadFile(filepath.Join(s.Dir, variant, file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, rewriteSegments(string(data), variant))
}

// rewriteSegments turns bare segment names into /hls/<variant>/<name>.
// Comments, blank lines, and already-absolute URIs are left untouched.
func rewriteSegments(in, variant string) string {
	var out strings.Builder
	for _, line := range strings.Split(in, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") ||
			strings.HasPrefix(t, "/") || strings.HasPrefix(t, "http") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&out, "/hls/%s/%s\n", variant, t)
	}
	return out.String()
}

type Snapshot struct {
	Available bool      `json:"available"`
	Master    string    `json:"master,omitempty"`
	Variants  []Variant `json:"variants,omitempty"`
}

func (s *Server) Snapshot() Snapshot {
	out := Snapshot{Master: "/hls/master.m3u8"}
	for _, v := range s.Variants {
		if _, err := os.Stat(filepath.Join(s.Dir, v.Name, "playlist.m3u8")); err == nil {
			out.Variants = append(out.Variants, v)
		}
	}
	out.Available = len(out.Variants) > 0
	return out
}

func (s *Server) Available() bool { return s.Snapshot().Available }
