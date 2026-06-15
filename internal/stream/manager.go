// Package stream manages per-channel ffmpeg processes that remux the
// HDHomeRun's MPEG-TS output into HLS for browser playback.
package stream

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config controls the session manager.
type Config struct {
	FFmpegPath  string        // path to ffmpeg binary
	FFmpegLog   string        // ffmpeg -loglevel (e.g. "warning", "info", "verbose")
	WorkDir     string        // base dir for per-channel HLS output
	TunerLimit  int           // max concurrent sessions (HDHR tuner count)
	IdleTimeout time.Duration // reap a session after no segment access for this long
	StartupWait time.Duration // how long to wait for the first playlist to appear
}

// Manager owns the live ffmpeg sessions.
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*session // keyed by sanitized channel id
}

type session struct {
	id        string
	srcURL    string // the HDHR URL (incl. ?transcode=) this session is streaming
	dir       string
	cancel    context.CancelFunc
	done      chan struct{}
	tunerBusy atomic.Bool // set if ffmpeg reported the device had no free tuner
	mu        sync.Mutex
	lastSeen  time.Time
}

func NewManager(cfg Config) *Manager {
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if cfg.StartupWait == 0 {
		cfg.StartupWait = 20 * time.Second // OTA tuner lock + transcoder spin-up can be slow
	}
	if cfg.TunerLimit == 0 {
		cfg.TunerLimit = 2
	}
	if cfg.FFmpegLog == "" {
		cfg.FFmpegLog = "warning"
	}
	m := &Manager{cfg: cfg, sessions: make(map[string]*session)}
	go m.reapLoop()
	return m
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// sanitizeID makes a channel id (e.g. "5.1") safe for use as a directory name.
func sanitizeID(id string) string {
	return unsafeChars.ReplaceAllString(id, "_")
}

// ErrTunersBusy is returned when all of our own sessions are in use (we've hit
// the configured tuner limit before even asking the device).
var ErrTunersBusy = fmt.Errorf("all tuners busy")

// ErrTunerUnavailable is returned when the HDHomeRun itself had no free tuner —
// e.g. a DVR recording or another app is using it. Detected from ffmpeg failing
// to open the input with a server error.
var ErrTunerUnavailable = fmt.Errorf("no tuner available on device")

// EnsurePlaylist starts (or reuses) a session for the given channel and returns
// the absolute path to its HLS playlist once it is ready to serve.
//
// srcURL is the HDHomeRun stream URL (already including the ?transcode= param).
func (m *Manager) EnsurePlaylist(srcURL, channelID string) (string, error) {
	id := sanitizeID(channelID)

	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok && s.srcURL != srcURL {
		// The profile (or URL) changed — tear down and re-tune at the new one.
		log.Printf("[stream %s] re-tuning (profile changed)", id)
		s.cancel()
		os.RemoveAll(s.dir)
		delete(m.sessions, id)
		ok = false
	}
	if !ok {
		if len(m.sessions) >= m.cfg.TunerLimit {
			m.mu.Unlock()
			return "", ErrTunersBusy
		}
		var err error
		s, err = m.startSession(srcURL, id)
		if err != nil {
			m.mu.Unlock()
			return "", err
		}
		m.sessions[id] = s
	}
	m.mu.Unlock()

	s.touch()

	// Wait until the playlist has enough segments that the player can start with
	// a healthy buffer instead of stalling at the live edge on a cold start.
	const readySegments = 3
	playlist := filepath.Join(s.dir, "index.m3u8")
	deadline := time.Now().Add(m.cfg.StartupWait)
	for {
		if playlistSegments(playlist) >= readySegments {
			return playlist, nil
		}
		select {
		case <-s.done:
			if s.tunerBusy.Load() {
				return "", ErrTunerUnavailable
			}
			return "", fmt.Errorf("ffmpeg for channel %s exited before producing a playlist", channelID)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			// Slow tuner: serve whatever is playable rather than failing.
			if playlistSegments(playlist) >= 1 {
				return playlist, nil
			}
			return "", fmt.Errorf("timed out waiting for channel %s to start", channelID)
		}
	}
}

// playlistSegments returns the number of segments currently listed in the HLS
// playlist at path (0 if it doesn't exist yet).
func playlistSegments(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return bytes.Count(data, []byte("#EXTINF"))
}

// indicatesTunerBusy reports whether an ffmpeg stderr line shows the HDHomeRun
// refused the stream because it had no free tuner. The device answers the stream
// request with a 503, which ffmpeg surfaces as a server-error opening the input.
func indicatesTunerBusy(line string) bool {
	return strings.Contains(line, "503") ||
		strings.Contains(line, "HTTP error 5") ||
		strings.Contains(line, "Server returned 5")
}

// SegmentPath returns the absolute path to a named segment/playlist file within
// a session's directory, refreshing the session's activity timestamp. It returns
// ("", false) if there is no active session for the channel.
func (m *Manager) SegmentPath(channelID, name string) (string, bool) {
	id := sanitizeID(channelID)
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return "", false
	}
	s.touch()
	// filepath.Base guards against path traversal in the requested name.
	return filepath.Join(s.dir, filepath.Base(name)), true
}

func (m *Manager) startSession(srcURL, id string) (*session, error) {
	dir := filepath.Join(m.cfg.WorkDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating work dir: %w", err)
	}
	// Clear any stale files from a previous run.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"-hide_banner", "-loglevel", m.cfg.FFmpegLog, "-nostats",
		// Fail fast instead of hanging forever if the tuner sends no data.
		"-rw_timeout", "10000000", // 10s (microseconds) with no read -> error
		"-i", srcURL,
		// The HDHomeRun transcodes video to H.264 but passes the original audio
		// through as AC-3, which browsers can't decode over HLS. Copy the video
		// (free) and re-encode just the audio to stereo AAC.
		"-c:v", "copy",
		"-c:a", "aac", "-ac", "2", "-b:a", "128k",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "20", // ~40s window so a brief VPN dip doesn't starve the player
		"-hls_flags", "delete_segments+independent_segments+omit_endlist",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(dir, "seg%05d.ts"),
		filepath.Join(dir, "index.m3u8"),
	}
	s := &session{
		id:       id,
		srcURL:   srcURL,
		dir:      dir,
		cancel:   cancel,
		done:     make(chan struct{}),
		lastSeen: time.Now(),
	}

	cmd := exec.CommandContext(ctx, m.cfg.FFmpegPath, args...)
	cmd.Stderr = newLogWriter(id, func(line string) {
		if indicatesTunerBusy(line) {
			s.tunerBusy.Store(true)
		}
	})

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}
	log.Printf("[stream %s] started ffmpeg (pid %d)", id, cmd.Process.Pid)

	go func() {
		err := cmd.Wait()
		log.Printf("[stream %s] ffmpeg exited: %v", id, err)
		close(s.done)
		// Drop a dead session so the next request re-tunes a fresh tuner instead
		// of serving its stale, no-longer-advancing playlist. Only remove it if
		// it's still the active session for this id (not already replaced).
		m.mu.Lock()
		if m.sessions[id] == s {
			os.RemoveAll(s.dir)
			delete(m.sessions, id)
		}
		m.mu.Unlock()
	}()
	return s, nil
}

func (s *session) touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *session) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastSeen)
}

func (m *Manager) reapLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		for id, s := range m.sessions {
			select {
			case <-s.done:
				// ffmpeg already exited.
			default:
				if s.idle() < m.cfg.IdleTimeout {
					continue
				}
			}
			log.Printf("[stream %s] reaping (idle %s)", id, s.idle().Round(time.Second))
			s.cancel()
			os.RemoveAll(s.dir)
			delete(m.sessions, id)
		}
		m.mu.Unlock()
	}
}

// Shutdown stops all sessions.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.cancel()
		os.RemoveAll(s.dir)
		delete(m.sessions, id)
	}
}
