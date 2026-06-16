// Package server wires the HDHomeRun client and stream manager to HTTP handlers.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"hdhrstream/internal/hdhr"
	"hdhrstream/internal/stream"
)

// Config for the HTTP server.
type Config struct {
	Client          *hdhr.Client
	Manager         *stream.Manager
	WebFS           fs.FS    // embedded SPA assets
	DefaultProfile  string   // default transcode profile, e.g. "mobile"
	Profiles        []string // profiles to offer in the UI, in display order
	AllowedProfiles map[string]bool
	Debug           bool // verbose request logging + browser console tracing
}

// New returns an http.Handler serving the API and SPA.
func New(cfg Config) http.Handler {
	s := &srv{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/channels", s.handleChannels)
	mux.HandleFunc("GET /playlist.m3u", s.handlePlaylist)
	mux.HandleFunc("GET /stream/{ch}/{file}", s.handleStream)
	// no-cache so a new build's app.js/styles.css/index.html are picked up on the
	// next load instead of a stale cached copy (the browser still revalidates and
	// gets a 304 when nothing changed).
	mux.Handle("GET /", noCacheStatic(http.FileServerFS(cfg.WebFS)))
	return logRequests(mux, cfg.Debug)
}

func noCacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// logRequests logs each request's method, path, status, duration and client. In
// debug mode it logs every request (so we can see how players behave); otherwise
// it logs only failures (status >= 400).
func logRequests(next http.Handler, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if debug || rec.status >= 400 {
			log.Printf("%s %s -> %d (%s) client=%s ua=%q",
				r.Method, r.URL.RequestURI(), rec.status, time.Since(start).Round(time.Millisecond),
				clientIP(r), r.UserAgent())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// externalBase returns the public-facing base URL (scheme://host[/prefix]) the
// client used to reach us, honoring reverse-proxy headers so that generated
// absolute URLs are correct behind a proxy that terminates TLS and/or serves the
// app under a path prefix (e.g. a secret /s/TOKEN/). Apache should set
// X-Forwarded-Proto and X-Forwarded-Prefix; Host is preserved via
// ProxyPreserveHost.
func externalBase(r *http.Request) string {
	scheme := firstField(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := firstField(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	prefix := strings.TrimRight(firstField(r.Header.Get("X-Forwarded-Prefix")), "/")
	return scheme + "://" + host + prefix
}

// firstField returns the first comma-separated value of a header, trimmed
// (proxies may chain multiple values).
func firstField(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

type srv struct {
	cfg Config
}

type channelDTO struct {
	Number   string `json:"number"`
	Name     string `json:"name"`
	HD       bool   `json:"hd"`
	Favorite bool   `json:"favorite"`
}

func (s *srv) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"defaultProfile": s.cfg.DefaultProfile,
		"profiles":       s.cfg.Profiles,
		"debug":          s.cfg.Debug,
	})
}

func (s *srv) handleChannels(w http.ResponseWriter, r *http.Request) {
	chans, err := s.cfg.Client.Lineup(r.Context())
	if err != nil {
		log.Printf("lineup error: %v", err)
		http.Error(w, "could not reach HDHomeRun", http.StatusBadGateway)
		return
	}
	out := make([]channelDTO, 0, len(chans))
	for _, c := range chans {
		if c.DRM != 0 { // can't stream encrypted channels
			continue
		}
		out = append(out, channelDTO{Number: c.GuideNumber, Name: c.GuideName, HD: c.HD != 0, Favorite: c.Favorite != 0})
	}
	writeJSON(w, out)
}

// handlePlaylist emits an M3U playlist (one HLS entry per channel) for use in
// IPTV players such as VLC or Infuse on Apple TV. The transcode profile is taken
// from ?profile= (falling back to the server default) and baked into each URL.
func (s *srv) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	chans, err := s.cfg.Client.Lineup(r.Context())
	if err != nil {
		log.Printf("lineup error: %v", err)
		http.Error(w, "could not reach HDHomeRun", http.StatusBadGateway)
		return
	}

	profile := s.profile(r)
	base := externalBase(r)

	var favs, others []hdhr.Channel
	for _, c := range chans {
		if c.DRM != 0 { // can't stream encrypted channels
			continue
		}
		if c.Favorite != 0 {
			favs = append(favs, c)
		} else {
			others = append(others, c)
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	write := func(c hdhr.Channel, group string) {
		// Absolute URL: IPTV players (e.g. Bro on Apple TV) store playlist entries
		// detached from the playlist's own URL, so relative entries never resolve.
		// externalBase honors reverse-proxy headers so the scheme, host and any
		// secret path prefix are correct. The HLS segments inside index.m3u8 stay
		// relative and are resolved by the player against this URL.
		streamURL := fmt.Sprintf("%s/stream/%s/index.m3u8?profile=%s",
			base, url.PathEscape(c.GuideNumber), url.QueryEscape(profile))
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-chno=%q tvg-name=%q group-title=%q,%s %s\n%s\n",
			c.GuideNumber, c.GuideName, group, c.GuideNumber, c.GuideName, streamURL)
	}
	for _, c := range favs {
		write(c, "Favorites")
	}
	for _, c := range others {
		write(c, "Channels")
	}

	w.Header().Set("Content-Type", "audio/x-mpegurl")
	io.WriteString(w, b.String())
}

func (s *srv) handleStream(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("ch")
	file := r.PathValue("file")

	switch {
	case file == "index.m3u8":
		s.servePlaylist(w, r, ch)
	case strings.HasSuffix(file, ".ts"):
		s.serveSegment(w, r, ch, file)
	default:
		http.NotFound(w, r)
	}
}

func (s *srv) servePlaylist(w http.ResponseWriter, r *http.Request, ch string) {
	profile := s.profile(r)
	srcURL, ok := s.sourceURL(r.Context(), ch, profile)
	if !ok {
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}

	path, err := s.cfg.Manager.EnsurePlaylist(srcURL, ch)
	switch err {
	case nil:
		// proceed
	case stream.ErrTunersBusy, stream.ErrTunerUnavailable:
		// Our own limit, or the device itself had no free tuner (e.g. a DVR
		// recording). Either way: 503 so the client can say "tuners busy".
		http.Error(w, "all tuners are in use", http.StatusServiceUnavailable)
		return
	default:
		log.Printf("playlist error for %s: %v", ch, err)
		http.Error(w, "could not start stream", http.StatusBadGateway)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("reading playlist for %s: %v", ch, err)
		http.Error(w, "could not read stream", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(withStartTag(data))
}

// startTag tells players to begin a few seconds back from the live edge, so
// they start with a little buffer ahead instead of at the bleeding edge.
var startTag = []byte("#EXT-X-START:TIME-OFFSET=-4\n")

// withStartTag injects an #EXT-X-START tag right after the #EXTM3U header.
func withStartTag(playlist []byte) []byte {
	const header = "#EXTM3U\n"
	if !bytes.HasPrefix(playlist, []byte(header)) || bytes.Contains(playlist, []byte("#EXT-X-START")) {
		return playlist
	}
	out := make([]byte, 0, len(playlist)+len(startTag))
	out = append(out, header...)
	out = append(out, startTag...)
	out = append(out, playlist[len(header):]...)
	return out
}

func (s *srv) serveSegment(w http.ResponseWriter, r *http.Request, ch, file string) {
	path, ok := s.cfg.Manager.SegmentPath(ch, file)
	if !ok {
		http.Error(w, "no active stream", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	http.ServeFile(w, r, path)
}

// sourceURL looks up the channel's HDHomeRun URL and appends the transcode profile.
func (s *srv) sourceURL(ctx context.Context, channelNumber, profile string) (string, bool) {
	chans, err := s.cfg.Client.Lineup(ctx)
	if err != nil {
		log.Printf("lineup error resolving %s: %v", channelNumber, err)
		return "", false
	}
	for _, c := range chans {
		if c.GuideNumber == channelNumber {
			sep := "?"
			if strings.Contains(c.URL, "?") {
				sep = "&"
			}
			return c.URL + sep + "transcode=" + profile, true
		}
	}
	return "", false
}

func (s *srv) profile(r *http.Request) string {
	p := r.URL.Query().Get("profile")
	if p != "" && s.cfg.AllowedProfiles[p] {
		return p
	}
	return s.cfg.DefaultProfile
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}
