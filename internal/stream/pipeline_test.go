package stream

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"
)

// requireFFmpeg skips the test unless ffmpeg is available and we're not in
// -short mode (these tests spawn ffmpeg and run in real time).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping ffmpeg pipeline test in -short mode")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping pipeline test")
	}
}

// fakeDevice emulates the HDHomeRun stream endpoint. With status 200 it streams
// a live H.264/AAC MPEG-TS test pattern (via ffmpeg); otherwise it just returns
// the given status, simulating a device with no free tuner.
func fakeDevice(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		cmd := exec.CommandContext(r.Context(), "ffmpeg",
			"-hide_banner", "-loglevel", "error", "-re",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-g", "30", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "64k",
			"-f", "mpegts", "pipe:1",
		)
		cmd.Stdout = w
		cmd.Run()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsurePlaylistProducesSegments(t *testing.T) {
	requireFFmpeg(t)

	device := fakeDevice(t, http.StatusOK)
	m := NewManager(Config{
		FFmpegPath:  "ffmpeg",
		WorkDir:     t.TempDir(),
		TunerLimit:  2,
		StartupWait: 30 * time.Second,
	})
	defer m.Shutdown()

	path, err := m.EnsurePlaylist(device.URL+"/auto/v1.1", "1.1")
	if err != nil {
		t.Fatalf("EnsurePlaylist: %v", err)
	}
	if n := playlistSegments(path); n < 3 {
		t.Errorf("expected playlist with >=3 segments, got %d", n)
	}
}

func TestEnsurePlaylistTunerUnavailable(t *testing.T) {
	requireFFmpeg(t)

	device := fakeDevice(t, http.StatusServiceUnavailable)
	m := NewManager(Config{
		FFmpegPath:  "ffmpeg",
		WorkDir:     t.TempDir(),
		TunerLimit:  2,
		StartupWait: 10 * time.Second,
	})
	defer m.Shutdown()

	_, err := m.EnsurePlaylist(device.URL+"/auto/v1.1", "1.1")
	if err != ErrTunerUnavailable {
		t.Fatalf("expected ErrTunerUnavailable, got %v", err)
	}
}

// fakeRecording serves a short, finite MPEG-2/AC-3 clip (like a real DVR
// recording) as fast as possible, so the transcode test runs quickly.
func fakeRecording(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		cmd := exec.CommandContext(r.Context(), "ffmpeg",
			"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=15",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=15",
			"-c:v", "mpeg2video", "-c:a", "ac3",
			"-f", "mpegts", "pipe:1",
		)
		cmd.Stdout = w
		cmd.Run()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVODTranscode(t *testing.T) {
	requireFFmpeg(t)

	device := fakeRecording(t)
	m := NewVODManager(VODConfig{
		FFmpegPath:  "ffmpeg",
		WorkDir:     t.TempDir(),
		Limit:       2,
		StartupWait: 30 * time.Second,
	})
	defer m.Shutdown()

	path, err := m.EnsurePlaylist(device.URL+"/recorded/play?id=x", "x", "internet480")
	if err != nil {
		t.Fatalf("EnsurePlaylist: %v", err)
	}
	if n := playlistSegments(path); n < 2 {
		t.Errorf("expected transcoded recording with >=2 segments, got %d", n)
	}
}

func TestEnsurePlaylistTunerLimit(t *testing.T) {
	requireFFmpeg(t)

	device := fakeDevice(t, http.StatusOK)
	m := NewManager(Config{
		FFmpegPath:  "ffmpeg",
		WorkDir:     t.TempDir(),
		TunerLimit:  1, // only one tuner
		StartupWait: 30 * time.Second,
	})
	defer m.Shutdown()

	// First channel takes the only tuner.
	if _, err := m.EnsurePlaylist(device.URL+"/auto/v1.1", "1.1"); err != nil {
		t.Fatalf("first channel: %v", err)
	}
	// Second distinct channel must be refused.
	if _, err := m.EnsurePlaylist(device.URL+"/auto/v2.2", "2.2"); err != ErrTunersBusy {
		t.Fatalf("expected ErrTunersBusy for second channel, got %v", err)
	}
}
