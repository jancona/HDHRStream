// Command hdhrstream serves a small web app for watching HDHomeRun channels
// remotely over HLS.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hdhrstream/internal/dvr"
	"hdhrstream/internal/hdhr"
	"hdhrstream/internal/server"
	"hdhrstream/internal/stream"
)

//go:embed all:web
var webAssets embed.FS

// allowedProfiles are the HDHomeRun transcode profiles we expose in the UI.
// See https://info.hdhomerun.com/info/http_api
var allowedProfiles = []string{"heavy", "mobile", "internet720", "internet480", "internet360", "internet240"}

func main() {
	var (
		listen     = flag.String("listen", envOr("HDHR_LISTEN", ":8080"), "address to listen on")
		hdhrURL    = flag.String("hdhr", envOr("HDHR_URL", ""), "HDHomeRun base URL, e.g. http://192.168.1.10")
		profile    = flag.String("profile", envOr("HDHR_PROFILE", "heavy"), "default transcode profile")
		workDir    = flag.String("workdir", envOr("HDHR_WORKDIR", filepath.Join(os.TempDir(), "hdhrstream")), "scratch dir for HLS output")
		ffmpegPath = flag.String("ffmpeg", envOr("HDHR_FFMPEG", "ffmpeg"), "path to ffmpeg binary")
		ffmpegLog  = flag.String("ffmpeg-loglevel", envOr("HDHR_FFMPEG_LOGLEVEL", "warning"), "ffmpeg -loglevel (warning, info, verbose)")
		debug      = flag.Bool("debug", envBool("HDHR_DEBUG"), "verbose debugging: per-request server logs, ffmpeg verbose, browser console tracing")
		dvrURL     = flag.String("dvr", envOr("HDHR_DVR", ""), "HDHomeRun RECORD engine URL for DVR playback, e.g. http://192.168.1.140:65001 (optional)")
		recWorkDir = flag.String("rec-workdir", envOr("HDHR_REC_WORKDIR", filepath.Join(os.TempDir(), "hdhrstream-rec")), "disk-backed scratch for transcoded recordings (do not use tmpfs)")
	)
	flag.Parse()

	if *hdhrURL == "" {
		log.Fatal("HDHomeRun URL required: set -hdhr or HDHR_URL (e.g. http://192.168.1.10)")
	}

	// In debug mode, default ffmpeg to verbose unless the operator chose a level.
	ffLog := *ffmpegLog
	if *debug && ffLog == "warning" {
		ffLog = "verbose"
	}

	client := hdhr.New(*hdhrURL)

	// Confirm we can reach the device and report its tuner count.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	dev, err := client.Discover(ctx)
	cancel()
	tunerLimit := 2
	if err != nil {
		log.Printf("warning: could not reach HDHomeRun at startup: %v", err)
	} else {
		log.Printf("connected to %q (%s, firmware %s), %d tuners", dev.FriendlyName, dev.ModelNumber, dev.FirmwareVersion, dev.TunerCount)
		if dev.TunerCount > 0 {
			tunerLimit = dev.TunerCount
		}
	}

	mgr := stream.NewManager(stream.Config{
		FFmpegPath: *ffmpegPath,
		FFmpegLog:  ffLog,
		WorkDir:    *workDir,
		TunerLimit: tunerLimit,
	})
	defer mgr.Shutdown()

	// Optional DVR (recordings) support.
	var dvrClient *dvr.Client
	var vodMgr *stream.VODManager
	if *dvrURL != "" {
		dvrClient = dvr.New(*dvrURL)
		vodMgr = stream.NewVODManager(stream.VODConfig{
			FFmpegPath: *ffmpegPath,
			FFmpegLog:  ffLog,
			WorkDir:    *recWorkDir,
		})
		defer vodMgr.Shutdown()
		log.Printf("DVR recordings enabled (%s), scratch %s", *dvrURL, *recWorkDir)
	}

	webFS, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatalf("web assets: %v", err)
	}

	allowed := make(map[string]bool, len(allowedProfiles))
	for _, p := range allowedProfiles {
		allowed[p] = true
	}
	if !allowed[*profile] {
		log.Fatalf("invalid default profile %q; choose one of %s", *profile, strings.Join(allowedProfiles, ", "))
	}

	handler := server.New(server.Config{
		Client:          client,
		Manager:         mgr,
		DVR:             dvrClient,
		VOD:             vodMgr,
		WebFS:           webFS,
		DefaultProfile:  *profile,
		Profiles:        allowedProfiles,
		AllowedProfiles: allowed,
		Debug:           *debug,
	})

	srv := &http.Server{Addr: *listen, Handler: handler}

	go func() {
		log.Printf("listening on %s (default profile %q)", *listen, *profile)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
