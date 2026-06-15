// Package dvr is a small client for the HDHomeRun RECORD engine's (undocumented
// but stable) HTTP API, used to list and play DVR recordings.
//
// Flow: <base>/discover.json advertises a StorageURL (recorded_files.json),
// which lists recorded series; each series' EpisodesURL lists episodes, and each
// episode has a PlayURL that streams the (full-quality MPEG-2) recording.
package dvr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a single RECORD engine at base (e.g. "http://192.168.1.140:65001").
type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Series is a recorded series group from recorded_files.json.
type Series struct {
	SeriesID  string `json:"SeriesID"`
	Title     string `json:"Title"`
	Category  string `json:"Category"`
	ImageURL  string `json:"ImageURL"`
	StartTime int64  `json:"StartTime"`
	New       int    `json:"New"`
}

// Episode is a single recording.
type Episode struct {
	Title           string `json:"Title"`
	EpisodeNumber   string `json:"EpisodeNumber"`
	EpisodeTitle    string `json:"EpisodeTitle"`
	Synopsis        string `json:"Synopsis"`
	ChannelName     string `json:"ChannelName"`
	ChannelNumber   string `json:"ChannelNumber"`
	ImageURL        string `json:"ImageURL"`
	StartTime       int64  `json:"StartTime"`
	EndTime         int64  `json:"EndTime"`
	RecordStartTime int64  `json:"RecordStartTime"`
	RecordEndTime   int64  `json:"RecordEndTime"`
	OriginalAirdate int64  `json:"OriginalAirdate"`
	PlayURL         string `json:"PlayURL"`
	CmdURL          string `json:"CmdURL"`
	Resume          int    `json:"Resume"`
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Series returns the recorded series groups.
func (c *Client) Series(ctx context.Context) ([]Series, error) {
	var s []Series
	if err := c.getJSON(ctx, c.base+"/recorded_files.json", &s); err != nil {
		return nil, err
	}
	return s, nil
}

// Episodes returns the recordings within a series.
func (c *Client) Episodes(ctx context.Context, seriesID string) ([]Episode, error) {
	u := c.base + "/recorded_files.json?SeriesID=" + url.QueryEscape(seriesID)
	var e []Episode
	if err := c.getJSON(ctx, u, &e); err != nil {
		return nil, err
	}
	return e, nil
}

// PlayURL reconstructs a recording's stream URL from its id (the id carried in
// the episode's PlayURL query string).
func (c *Client) PlayURL(id string) string {
	return c.base + "/recorded/play?id=" + url.QueryEscape(id)
}

// RecordingID extracts the id from an episode PlayURL
// (e.g. ".../recorded/play?id=c7439557ffaf7fe8" -> "c7439557ffaf7fe8").
func RecordingID(playURL string) string {
	if u, err := url.Parse(playURL); err == nil {
		return u.Query().Get("id")
	}
	return ""
}
