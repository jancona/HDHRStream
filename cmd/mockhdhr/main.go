// Command mockhdhr emulates just enough of the HDHomeRun HTTP API to smoke-test
// the server end to end: discover.json, lineup.json, and a continuous MPEG-TS
// stream (H.264/AAC test pattern via ffmpeg) at /auto/v<channel>.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
)

func main() {
	addr := flag.String("listen", ":9000", "address to listen on")
	flag.Parse()

	base := "http://localhost" + *addr

	http.HandleFunc("/discover.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"FriendlyName":"HDHR Mock","ModelNumber":"HDTC-2US","FirmwareName":"mock","FirmwareVersion":"0","DeviceID":"MOCK0001","TunerCount":2,"BaseURL":%q,"LineupURL":%q}`, base, base+"/lineup.json")
	})

	http.HandleFunc("/lineup.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[
  {"GuideNumber":"100.1","GuideName":"MOCK-HD","HD":1,"URL":%q},
  {"GuideNumber":"100.2","GuideName":"MOCK-2","URL":%q}
]`, base+"/auto/v100.1", base+"/auto/v100.2")
	})

	http.HandleFunc("/auto/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("stream request: %s?%s", r.URL.Path, r.URL.RawQuery)
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)

		// Generate a continuous H.264/AAC MPEG-TS test pattern to stdout.
		cmd := exec.CommandContext(r.Context(), "ffmpeg",
			"-hide_banner", "-loglevel", "error",
			"-re",
			"-f", "lavfi", "-i", "testsrc=size=640x360:rate=30",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-g", "60", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "128k",
			"-f", "mpegts", "pipe:1",
		)
		cmd.Stdout = w
		if err := cmd.Run(); err != nil {
			log.Printf("stream ended: %v", err)
		}
	})

	log.Printf("mock HDHomeRun listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
