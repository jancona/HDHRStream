package stream

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// VODConfig controls the recordings transcoder.
type VODConfig struct {
	FFmpegPath  string
	FFmpegLog   string
	WorkDir     string        // disk-backed scratch (recordings can be large; not tmpfs)
	Limit       int           // max concurrent transcodes (CPU bound, no tuners involved)
	IdleTimeout time.Duration // reap a session after no segment access for this long
	StartupWait time.Duration // how long to wait for the first segments
}

// VODManager transcodes DVR recordings (full-quality MPEG-2/AC-3) on the fly into
// a seekable HLS playlist for browser playback. Unlike live streams it uses no
// tuner; the only limit is CPU, so concurrency is capped separately.
type VODManager struct {
	cfg      VODConfig
	mu       sync.Mutex
	sessions map[string]*vodSession // keyed by recording id
}

type vodSession struct {
	profile  string // output profile this session was started with
	dir      string
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	lastSeen time.Time
}

// ErrBusy is returned when the concurrent-transcode limit is reached.
var ErrBusy = fmt.Errorf("transcoder busy")

// vodLadder maps a profile name to an output (scale height, video bitrate). We do
// the encoding ourselves since recordings are stored full-quality, so this is our
// own quality ladder (reusing the live profile names for a consistent UI).
var vodLadder = map[string][2]string{
	"internet240": {"240", "400k"},
	"internet360": {"360", "800k"},
	"internet480": {"480", "1500k"},
	"internet720": {"720", "2500k"},
	"mobile":      {"720", "2500k"},
	"heavy":       {"1080", "6000k"},
}

func NewVODManager(cfg VODConfig) *VODManager {
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if cfg.StartupWait == 0 {
		cfg.StartupWait = 30 * time.Second
	}
	if cfg.Limit == 0 {
		cfg.Limit = 2
	}
	if cfg.FFmpegLog == "" {
		cfg.FFmpegLog = "warning"
	}
	m := &VODManager{cfg: cfg, sessions: make(map[string]*vodSession)}
	go m.reapLoop()
	return m
}

// EnsurePlaylist starts (or reuses) a transcode of the recording at srcURL and
// returns the path to its HLS playlist once enough has been produced to start.
func (m *VODManager) EnsurePlaylist(srcURL, id, profile string) (string, error) {
	key := sanitizeID(id)

	m.mu.Lock()
	s, ok := m.sessions[key]
	if ok && s.profile != profile {
		s.cancel()
		os.RemoveAll(s.dir)
		delete(m.sessions, key)
		ok = false
	}
	if !ok {
		if len(m.sessions) >= m.cfg.Limit {
			m.mu.Unlock()
			return "", ErrBusy
		}
		var err error
		s, err = m.start(srcURL, key, profile)
		if err != nil {
			m.mu.Unlock()
			return "", err
		}
		m.sessions[key] = s
	}
	m.mu.Unlock()

	s.touch()

	const readySegments = 1 // a recording is VOD; one segment is enough to start
	playlist := filepath.Join(s.dir, "index.m3u8")
	deadline := time.Now().Add(m.cfg.StartupWait)
	for {
		if playlistSegments(playlist) >= readySegments {
			return playlist, nil
		}
		select {
		case <-s.done:
			if playlistSegments(playlist) >= 1 {
				return playlist, nil // short recording finished quickly
			}
			return "", fmt.Errorf("transcode of recording %s produced no output", id)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			if playlistSegments(playlist) >= 1 {
				return playlist, nil
			}
			return "", fmt.Errorf("timed out transcoding recording %s", id)
		}
	}
}

// SegmentPath returns the on-disk path of a playlist/segment file for an active
// recording session, refreshing its activity timestamp.
func (m *VODManager) SegmentPath(id, name string) (string, bool) {
	key := sanitizeID(id)
	m.mu.Lock()
	s, ok := m.sessions[key]
	m.mu.Unlock()
	if !ok {
		return "", false
	}
	s.touch()
	return filepath.Join(s.dir, filepath.Base(name)), true
}

// probeVideoCodec returns the recording's video codec (e.g. "h264", "mpeg2video")
// via ffprobe, or "" if it can't be determined. ffprobe is assumed to sit next to
// ffmpeg. On failure we return "" so the caller takes the safe transcode path.
func probeVideoCodec(ffmpegPath, srcURL string) string {
	probe := "ffprobe"
	if strings.ContainsAny(ffmpegPath, `/\`) {
		probe = filepath.Join(filepath.Dir(ffmpegPath), "ffprobe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, probe,
		"-v", "error", "-rw_timeout", "8000000",
		"-select_streams", "v:0", "-show_entries", "stream=codec_name",
		"-of", "default=nw=1:nk=1", srcURL).Output()
	if err != nil {
		return ""
	}
	// ffprobe may print one line per matching stream; take the first.
	codec := strings.TrimSpace(string(out))
	if i := strings.IndexAny(codec, "\r\n"); i >= 0 {
		codec = codec[:i]
	}
	return codec
}

func (m *VODManager) start(srcURL, key, profile string) (*vodSession, error) {
	dir := filepath.Join(m.cfg.WorkDir, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating work dir: %w", err)
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	args := []string{
		"-hide_banner", "-loglevel", m.cfg.FFmpegLog, "-nostats",
		"-rw_timeout", "15000000", // 15s with no data from the DVR -> fail instead of hanging
		// The DVR feeds recordings faster than real time, which makes the HLS
		// playlist grow at >1x; players then chase a runaway "live edge" and
		// freeze. Read at real time so it grows at playback speed, like live.
		"-re",
		"-i", srcURL,
	}
	// If the recording's video is already H.264, just copy it — a cheap real-time
	// remux like the live path. (The HDHomeRun DVR can be set to record the
	// Extend's transcoded H.264 output instead of the original MPEG-2.) Only
	// MPEG-2 (recorded at original quality) needs a full, CPU-heavy transcode.
	if codec := probeVideoCodec(m.cfg.FFmpegPath, srcURL); codec == "h264" {
		log.Printf("[rec %s] video is h264; copying (remux only)", key)
		args = append(args, "-c:v", "copy")
	} else {
		spec, ok := vodLadder[profile]
		if !ok {
			spec = vodLadder["internet480"]
		}
		height, vbitrate := spec[0], spec[1]
		log.Printf("[rec %s] video is %q; transcoding to %sp @ %s", key, codec, height, vbitrate)
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast",
			"-vf", "yadif,scale=-2:"+height,
			"-b:v", vbitrate, "-maxrate", vbitrate, "-bufsize", vbitrate,
			"-profile:v", "high", "-pix_fmt", "yuv420p",
			// Force a keyframe at each HLS segment boundary so segments are a
			// consistent ~6s (libx264's default GOP is too long). Time-based, so
			// it's framerate-independent.
			"-force_key_frames", "expr:gte(t,n_forced*6)",
		)
	}
	// Audio is AC-3, which browsers can't decode over HLS; always re-encode to
	// stereo AAC (cheap).
	args = append(args,
		"-c:a", "aac", "-ac", "2", "-b:a", "128k",
		"-f", "hls",
		"-hls_time", "6",
		"-hls_playlist_type", "event", // keep all segments + grow the list -> seekable VOD
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(dir, "seg%05d.ts"),
		filepath.Join(dir, "index.m3u8"),
	)
	cmd := exec.CommandContext(ctx, m.cfg.FFmpegPath, args...)
	cmd.Stderr = newLogWriter("rec:"+key, nil)

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}
	log.Printf("[rec %s] started transcode (pid %d, profile %s)", key, cmd.Process.Pid, profile)

	s := &vodSession{
		profile:  profile,
		dir:      dir,
		cancel:   cancel,
		done:     make(chan struct{}),
		lastSeen: time.Now(),
	}
	go func() {
		err := cmd.Wait()
		log.Printf("[rec %s] transcode exited: %v", key, err)
		close(s.done)
		m.mu.Lock()
		if m.sessions[key] == s {
			os.RemoveAll(s.dir)
			delete(m.sessions, key)
		}
		m.mu.Unlock()
	}()
	return s, nil
}

func (s *vodSession) touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *vodSession) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastSeen)
}

func (m *VODManager) reapLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		for key, s := range m.sessions {
			select {
			case <-s.done:
			default:
				if s.idle() < m.cfg.IdleTimeout {
					continue
				}
			}
			log.Printf("[rec %s] reaping (idle %s)", key, s.idle().Round(time.Second))
			s.cancel()
			os.RemoveAll(s.dir)
			delete(m.sessions, key)
		}
		m.mu.Unlock()
	}
}

// Shutdown stops all transcodes.
func (m *VODManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, s := range m.sessions {
		s.cancel()
		os.RemoveAll(s.dir)
		delete(m.sessions, key)
	}
}
