package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"hdhrstream/internal/hdhr"
)

const testLineup = `[
  {"GuideNumber":"6.1","GuideName":"WCSH-HD","HD":1,"Favorite":1,"URL":"http://device/auto/v6.1"},
  {"GuideNumber":"7.1","GuideName":"WPFO","URL":"http://device/auto/v7.1"},
  {"GuideNumber":"9.1","GuideName":"PAY-DRM","DRM":1,"URL":"http://device/auto/v9.1"}
]`

// newTestHandler returns the app handler wired to a fake HDHomeRun serving the
// lineup above.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lineup.json" {
			w.Write([]byte(testLineup))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(device.Close)

	return New(Config{
		Client:          hdhr.New(device.URL),
		WebFS:           fstest.MapFS{},
		DefaultProfile:  "heavy",
		Profiles:        []string{"heavy", "internet480"},
		AllowedProfiles: map[string]bool{"heavy": true, "internet480": true},
	})
}

func TestHandleChannelsFiltersDRMAndFlagsFavorites(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t).ServeHTTP(rec, httptest.NewRequest("GET", "/api/channels", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "9.1") || strings.Contains(body, "PAY-DRM") {
		t.Errorf("DRM channel should be filtered out:\n%s", body)
	}
	if !strings.Contains(body, `"number":"6.1"`) || !strings.Contains(body, `"favorite":true`) {
		t.Errorf("expected favorite 6.1 present:\n%s", body)
	}
}

func TestHandlePlaylistAbsoluteURLsBehindProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/playlist.m3u?profile=internet480", nil)
	req.Host = "home.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Prefix", "/TOKEN")
	rec := httptest.NewRecorder()
	newTestHandler(t).ServeHTTP(rec, req)

	body := rec.Body.String()

	want := "https://home.example.com/TOKEN/stream/6.1/index.m3u8?profile=internet480"
	if !strings.Contains(body, want) {
		t.Errorf("missing absolute prefixed URL %q in:\n%s", want, body)
	}
	if strings.Contains(body, "9.1") {
		t.Errorf("DRM channel should not be in playlist:\n%s", body)
	}
	// Favorites (6.1) must come before non-favorites (7.1).
	if i61, i71 := strings.Index(body, "6.1"), strings.Index(body, "7.1"); i61 == -1 || i71 == -1 || i61 > i71 {
		t.Errorf("favorites should come first; 6.1@%d 7.1@%d", i61, i71)
	}
	if !strings.Contains(body, `group-title="Favorites"`) || !strings.Contains(body, `group-title="Channels"`) {
		t.Errorf("missing group titles:\n%s", body)
	}
}

func TestHandlePlaylistInvalidProfileFallsBackToDefault(t *testing.T) {
	req := httptest.NewRequest("GET", "/playlist.m3u?profile=bogus", nil)
	req.Host = "h"
	rec := httptest.NewRecorder()
	newTestHandler(t).ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "profile=heavy") {
		t.Errorf("invalid profile should fall back to default 'heavy':\n%s", rec.Body.String())
	}
}

func TestExternalBase(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		headers map[string]string
		want    string
	}{
		{"direct", "h:8080", nil, "http://h:8080"},
		{"forwarded proto+prefix", "home.example.com",
			map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Prefix": "/TOKEN"},
			"https://home.example.com/TOKEN"},
		{"prefix trailing slash trimmed", "h",
			map[string]string{"X-Forwarded-Prefix": "/p/"}, "http://h/p"},
		{"forwarded host", "internal",
			map[string]string{"X-Forwarded-Host": "public.example.com", "X-Forwarded-Proto": "https"},
			"https://public.example.com"},
		{"comma-separated header takes first", "h",
			map[string]string{"X-Forwarded-Proto": "https, http"}, "https://h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Host = tt.host
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			if got := externalBase(req); got != tt.want {
				t.Errorf("externalBase = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithStartTag(t *testing.T) {
	in := "#EXTM3U\n#EXT-X-VERSION:6\n#EXTINF:2.0,\nseg0.ts\n"

	out := string(withStartTag([]byte(in)))
	if !strings.Contains(out, "#EXT-X-START:") {
		t.Errorf("start tag not injected:\n%s", out)
	}
	// Idempotent: don't inject twice if already present.
	if got := strings.Count(string(withStartTag([]byte(out))), "#EXT-X-START"); got != 1 {
		t.Errorf("start tag should appear once, got %d", got)
	}
	// Non-playlist input is left untouched.
	if got := string(withStartTag([]byte("garbage"))); got != "garbage" {
		t.Errorf("non-playlist modified: %q", got)
	}
}
