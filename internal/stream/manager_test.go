package stream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndicatesTunerBusy(t *testing.T) {
	busy := []string{
		"[http @ 0x123] HTTP error 503 Service Unavailable",
		"http://d/auto/v5.1: Server returned 503 Service Unavailable",
		"Server returned 502 Bad Gateway",
	}
	notBusy := []string{
		"Input #0, mpegts, from 'http://d/auto/v5.1'",
		"Stream #0:0: Video: h264 (High)",
		"[http @ 0x123] HTTP error 404 Not Found",
		"frame=  100 fps= 30 q=-1.0",
	}
	for _, l := range busy {
		if !indicatesTunerBusy(l) {
			t.Errorf("expected tuner-busy for %q", l)
		}
	}
	for _, l := range notBusy {
		if indicatesTunerBusy(l) {
			t.Errorf("expected NOT tuner-busy for %q", l)
		}
	}
}

func TestPlaylistSegments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "index.m3u8")

	if got := playlistSegments(p); got != 0 {
		t.Errorf("missing file should count 0, got %d", got)
	}

	os.WriteFile(p, []byte("#EXTM3U\n#EXTINF:2.0,\nseg0.ts\n#EXTINF:2.0,\nseg1.ts\n"), 0o644)
	if got := playlistSegments(p); got != 2 {
		t.Errorf("expected 2 segments, got %d", got)
	}
}

func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"5.1":    "5.1", // dots are allowed
		"a/b":    "a_b",
		"x y":    "x_y",
		"../etc": ".._etc",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}
