// Package hdhr is a small client for the HDHomeRun HTTP API.
// See https://info.hdhomerun.com/info/http_api
package hdhr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Device is the response from <base>/discover.json.
type Device struct {
	FriendlyName    string `json:"FriendlyName"`
	ModelNumber     string `json:"ModelNumber"`
	FirmwareName    string `json:"FirmwareName"`
	FirmwareVersion string `json:"FirmwareVersion"`
	DeviceID        string `json:"DeviceID"`
	TunerCount      int    `json:"TunerCount"`
	BaseURL         string `json:"BaseURL"`
	LineupURL       string `json:"LineupURL"`
}

// Channel is one entry from <base>/lineup.json.
type Channel struct {
	GuideNumber string `json:"GuideNumber"` // e.g. "5.1"
	GuideName   string `json:"GuideName"`   // e.g. "WNBC-HD"
	VideoCodec  string `json:"VideoCodec,omitempty"`
	AudioCodec  string `json:"AudioCodec,omitempty"`
	HD          int    `json:"HD,omitempty"`
	DRM         int    `json:"DRM,omitempty"`
	Favorite    int    `json:"Favorite,omitempty"`
	URL         string `json:"URL"` // e.g. "http://192.168.1.10:5004/auto/v5.1"
}

// Client talks to a single HDHomeRun device at baseURL (e.g. "http://192.168.1.10").
type Client struct {
	baseURL   string
	http      *http.Client
	lineupTTL time.Duration

	mu          sync.Mutex
	lineupCache []Channel
	lineupAt    time.Time
}

func New(baseURL string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      &http.Client{Timeout: 10 * time.Second},
		lineupTTL: 30 * time.Second,
	}
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	url := c.baseURL + path
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
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s: %w", url, err)
	}
	return nil
}

// Discover returns the device's identity and capabilities.
func (c *Client) Discover(ctx context.Context) (*Device, error) {
	var d Device
	if err := c.getJSON(ctx, "/discover.json", &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Lineup returns the list of tunable channels. Results are cached briefly so
// that frequent stream requests don't hammer the device.
func (c *Client) Lineup(ctx context.Context) ([]Channel, error) {
	c.mu.Lock()
	if c.lineupCache != nil && time.Since(c.lineupAt) < c.lineupTTL {
		cached := c.lineupCache
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	var chans []Channel
	if err := c.getJSON(ctx, "/lineup.json", &chans); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.lineupCache = chans
	c.lineupAt = time.Now()
	c.mu.Unlock()
	return chans, nil
}
