package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"hdhrstream/internal/dvr"
	"hdhrstream/internal/hdhr"
)

// newTestHandlerWithDVR wires the app to both a fake tuner and a fake DVR engine.
func newTestHandlerWithDVR(t *testing.T) http.Handler {
	t.Helper()
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lineup.json" {
			w.Write([]byte(testLineup))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(device.Close)

	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recorded_files.json" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("SeriesID") != "" {
			w.Write([]byte(`[{"Title":"Jeopardy!","EpisodeNumber":"S42E200","ChannelName":"WMTWDT","RecordStartTime":1,"RecordEndTime":1801,"PlayURL":"http://d/recorded/play?id=abc123"}]`))
			return
		}
		w.Write([]byte(`[{"SeriesID":"S1","Title":"Jeopardy!","ImageURL":"http://img/x.jpg"}]`))
	}))
	t.Cleanup(rec.Close)

	return New(Config{
		Client:          hdhr.New(device.URL),
		DVR:             dvr.New(rec.URL),
		WebFS:           fstest.MapFS{},
		DefaultProfile:  "heavy",
		Profiles:        []string{"heavy"},
		AllowedProfiles: map[string]bool{"heavy": true},
	})
}

func TestRecordingsConfigFlag(t *testing.T) {
	without := httptest.NewRecorder()
	newTestHandler(t).ServeHTTP(without, httptest.NewRequest("GET", "/api/config", nil))
	if strings.Contains(without.Body.String(), `"recordings":true`) {
		t.Errorf("recordings should be false without a DVR:\n%s", without.Body.String())
	}

	with := httptest.NewRecorder()
	newTestHandlerWithDVR(t).ServeHTTP(with, httptest.NewRequest("GET", "/api/config", nil))
	if !strings.Contains(with.Body.String(), `"recordings":true`) {
		t.Errorf("recordings should be true with a DVR:\n%s", with.Body.String())
	}
}

func TestHandleSeriesAndEpisodes(t *testing.T) {
	h := newTestHandlerWithDVR(t)

	series := httptest.NewRecorder()
	h.ServeHTTP(series, httptest.NewRequest("GET", "/api/recordings", nil))
	if !strings.Contains(series.Body.String(), `"title":"Jeopardy!"`) {
		t.Errorf("series list:\n%s", series.Body.String())
	}

	eps := httptest.NewRecorder()
	h.ServeHTTP(eps, httptest.NewRequest("GET", "/api/recordings/S1", nil))
	body := eps.Body.String()
	if !strings.Contains(body, `"id":"abc123"`) {
		t.Errorf("episode id should be extracted from PlayURL:\n%s", body)
	}
	if !strings.Contains(body, `"duration":1800`) {
		t.Errorf("duration should be RecordEnd-RecordStart:\n%s", body)
	}
}

func TestRecordingsRoutesAbsentWithoutDVR(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t).ServeHTTP(rec, httptest.NewRequest("GET", "/api/recordings", nil))
	// With no DVR the route isn't registered, so it falls through to the static
	// file server and 404s rather than returning recordings.
	if rec.Code == http.StatusOK {
		t.Errorf("expected non-200 for /api/recordings without a DVR, got %d", rec.Code)
	}
}
