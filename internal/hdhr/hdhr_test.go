package hdhr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDiscover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/discover.json" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"FriendlyName":"Test","ModelNumber":"HDTC-2US","TunerCount":2,"BaseURL":"http://d","LineupURL":"http://d/lineup.json"}`))
	}))
	defer srv.Close()

	d, err := New(srv.URL).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.FriendlyName != "Test" || d.ModelNumber != "HDTC-2US" || d.TunerCount != 2 {
		t.Fatalf("unexpected device: %+v", d)
	}
}

func TestLineupParsesAndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lineup.json" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Write([]byte(`[{"GuideNumber":"6.1","GuideName":"WCSH","HD":1,"Favorite":1,"URL":"http://d/auto/v6.1"}]`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	for i := 0; i < 3; i++ {
		chans, err := c.Lineup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(chans) != 1 || chans[0].GuideNumber != "6.1" || chans[0].Favorite != 1 {
			t.Fatalf("unexpected lineup: %+v", chans)
		}
	}
	// The 30s cache should mean only one upstream fetch for three calls.
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 upstream fetch (cached), got %d", got)
	}
}

func TestLineupErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := New(srv.URL).Lineup(context.Background()); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
