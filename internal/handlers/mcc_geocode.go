package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Reverse geocoding for the grievance bot via Nominatim (OpenStreetMap).
// The public instance needs no key but asks for an identifying User-Agent
// and at most one request per second, which complaint volume stays under.

const (
	mccDefaultGeocoderURL = "https://nominatim.openstreetmap.org"
	mccGeocoderUserAgent  = "MMP-Grievance-Bot/1.0 (https://kdelectrical.site)"
	mccGeocodeTimeout     = 5 * time.Second
)

func (a *App) reverseGeocodeMCC(ctx context.Context, lat, lon float64) (string, error) {
	base := a.Config.MCC.GeocoderURL
	if base == "" {
		base = mccDefaultGeocoderURL
	}
	q := url.Values{
		"format":          {"jsonv2"},
		"lat":             {fmt.Sprintf("%f", lat)},
		"lon":             {fmt.Sprintf("%f", lon)},
		"zoom":            {"18"},
		"accept-language": {"en"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/reverse?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", mccGeocoderUserAgent)

	client := a.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("geocoder returned %d", resp.StatusCode)
	}

	var out struct {
		DisplayName string `json:"display_name"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != "" || out.DisplayName == "" {
		return "", fmt.Errorf("no address found: %s", out.Error)
	}
	return strings.TrimSuffix(out.DisplayName, ", India"), nil
}
