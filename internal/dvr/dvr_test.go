package dvr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSeriesAndEpisodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recorded_files.json" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("SeriesID") == "C184056EN6FJY" {
			w.Write([]byte(`[{"Title":"Jeopardy!","EpisodeNumber":"S42E200","Synopsis":"A game show.","ChannelName":"WMTWDT","RecordStartTime":1781306970,"RecordEndTime":1781310600,"PlayURL":"http://d/recorded/play?id=c7439557ffaf7fe8","CmdURL":"http://d/recorded/cmd?id=c7439557ffaf7fe8"}]`))
			return
		}
		w.Write([]byte(`[{"SeriesID":"C184056EN6FJY","Title":"Jeopardy!","Category":"series","ImageURL":"http://img/x.jpg"}]`))
	}))
	defer srv.Close()

	c := New(srv.URL)

	series, err := c.Series(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Title != "Jeopardy!" || series[0].SeriesID != "C184056EN6FJY" {
		t.Fatalf("unexpected series: %+v", series)
	}

	eps, err := c.Episodes(context.Background(), "C184056EN6FJY")
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 episode, got %d", len(eps))
	}
	e := eps[0]
	if e.EpisodeNumber != "S42E200" || e.ChannelName != "WMTWDT" {
		t.Errorf("unexpected episode: %+v", e)
	}
	if id := RecordingID(e.PlayURL); id != "c7439557ffaf7fe8" {
		t.Errorf("RecordingID = %q, want c7439557ffaf7fe8", id)
	}
}

func TestRecordingIDAndPlayURL(t *testing.T) {
	if got := RecordingID("http://d:65001/recorded/play?id=abc123"); got != "abc123" {
		t.Errorf("RecordingID = %q, want abc123", got)
	}
	if got := RecordingID("not a url with no id"); got != "" {
		t.Errorf("RecordingID of junk = %q, want empty", got)
	}
	if got := New("http://d:65001/").PlayURL("abc123"); got != "http://d:65001/recorded/play?id=abc123" {
		t.Errorf("PlayURL = %q", got)
	}
}
