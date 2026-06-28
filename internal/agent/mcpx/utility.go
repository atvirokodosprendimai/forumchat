package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	// time/tzdata embeds the IANA zoneinfo database into the binary. The release
	// image is gcr.io/distroless/static, which ships NO /usr/share/zoneinfo, so
	// time.LoadLocation would fail for every name and the current_datetime
	// tool's `timezone` argument would be dead in prod. Embedding costs ~450KB
	// of binary size, which is the right trade for a tool whose whole point is
	// answering "what time is it (over there)".
	_ "time/tzdata"
)

// This file holds the two community-agnostic utility tools the in-process MCP
// server exposes to tools-enabled agents: `current_datetime` and `weather`.
// Unlike search/list_issues (internal_session, community-scoped DB reads), these
// take NO community id and read NO tenant data — they leak nothing, so they are
// registered unconditionally rather than gated behind a wired closure.

// --- current_datetime -----------------------------------------------------

// datetimeInput is the current_datetime tool's parameter schema. Timezone is
// optional; an empty value means UTC.
type datetimeInput struct {
	Timezone string `json:"timezone,omitempty" jsonschema:"optional IANA timezone name (e.g. \"Europe/Vilnius\", \"America/New_York\"); defaults to UTC"`
}

// formatDateTime renders now in the requested IANA timezone (UTC when tz is
// empty). It is split out from the tool handler so the formatting and the
// timezone-resolution branch are unit-testable without standing up an MCP
// server. A bad timezone is returned as an error so the caller can relay a
// helpful message to the model rather than silently falling back to UTC.
func formatDateTime(now time.Time, tz string) (string, error) {
	loc := time.UTC
	name := "UTC"
	if tz = strings.TrimSpace(tz); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return "", fmt.Errorf("unknown timezone %q — use an IANA name like \"Europe/Vilnius\" or \"America/New_York\"", tz)
		}
		loc, name = l, tz
	}
	t := now.In(loc)
	// RFC3339 is the machine-friendly anchor; the weekday + unix line gives the
	// model everything it needs to reason about "today", "tomorrow", deltas, etc.
	return fmt.Sprintf(
		"Current date & time: %s (%s)\nTimezone: %s\nDay of week: %s\nUnix seconds: %d",
		t.Format("2006-01-02 15:04:05"), t.Format("Mon, 02 Jan 2006 15:04 MST"),
		name, t.Weekday(), t.Unix(),
	), nil
}

// --- weather --------------------------------------------------------------

// weatherInput is the weather tool's parameter schema.
type weatherInput struct {
	Location string `json:"location" jsonschema:"a place name to look up, e.g. \"Vilnius\", \"London, UK\", \"San Francisco\""`
}

// Open-Meteo endpoints. Free, no API key, and the host is a fixed trusted
// constant (not user-supplied), so no SSRF guard (netguard) is needed. They are
// package vars rather than consts purely so tests can point them at an httptest
// stub.
var (
	geocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	forecastURL = "https://api.open-meteo.com/v1/forecast"
)

// weatherHTTP is the shared client for the two Open-Meteo calls. The 10s timeout
// matches the webhook relay's outbound convention and bounds a slow upstream so
// a weather lookup can never hang an agent generation indefinitely.
var weatherHTTP = &http.Client{Timeout: 10 * time.Second}

// geoResult is one match from the Open-Meteo geocoding API. Only the fields we
// render are decoded; the rest of the (large) payload is ignored.
type geoResult struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Country   string  `json:"country"`
	Admin1    string  `json:"admin1"` // state / region, e.g. "England"
}

// forecastResult mirrors the subset of the Open-Meteo forecast payload we ask
// for via the `current=` query parameter.
type forecastResult struct {
	Current struct {
		Time        string  `json:"time"`
		Temperature float64 `json:"temperature_2m"`
		Apparent    float64 `json:"apparent_temperature"`
		Humidity    int     `json:"relative_humidity_2m"`
		WeatherCode int     `json:"weather_code"`
		WindSpeed   float64 `json:"wind_speed_10m"`
	} `json:"current"`
	CurrentUnits struct {
		Temperature string `json:"temperature_2m"`
		WindSpeed   string `json:"wind_speed_10m"`
	} `json:"current_units"`
}

// fetchWeather geocodes location and returns a human-readable current-conditions
// summary for the first match. It is the testable core of the weather tool:
// tests redirect geocodeURL/forecastURL at a stub server and exercise it
// directly. The two-step (geocode → forecast) shape is Open-Meteo's API; a
// location with no geocoding match is a normal "not found", not an error of ours.
func fetchWeather(ctx context.Context, location string) (string, error) {
	if strings.TrimSpace(location) == "" {
		return "", fmt.Errorf("location is required")
	}

	// Step 1 — resolve the place name to coordinates.
	geo := url.Values{}
	geo.Set("name", location)
	geo.Set("count", "1")
	geo.Set("language", "en")
	geo.Set("format", "json")
	var geoResp struct {
		Results []geoResult `json:"results"`
	}
	if err := getJSON(ctx, geocodeURL+"?"+geo.Encode(), &geoResp); err != nil {
		return "", fmt.Errorf("geocode %q: %w", location, err)
	}
	if len(geoResp.Results) == 0 {
		return fmt.Sprintf("No place matching %q was found.", location), nil
	}
	place := geoResp.Results[0]

	// Step 2 — current conditions at those coordinates. timezone=auto makes the
	// returned observation time local to the place, which is what a reader expects.
	fc := url.Values{}
	fc.Set("latitude", strconv.FormatFloat(place.Latitude, 'f', -1, 64))
	fc.Set("longitude", strconv.FormatFloat(place.Longitude, 'f', -1, 64))
	fc.Set("current", "temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m")
	fc.Set("timezone", "auto")
	var forecast forecastResult
	if err := getJSON(ctx, forecastURL+"?"+fc.Encode(), &forecast); err != nil {
		return "", fmt.Errorf("forecast for %s: %w", place.Name, err)
	}
	// Open-Meteo always stamps current.time when a current block is present. An
	// empty value means the (still HTTP-200) body had no current conditions —
	// don't fabricate plausible zero-value weather (0°C / WMO code 0 = "Clear
	// sky"); surface it as an error so the model says so instead of lying.
	if strings.TrimSpace(forecast.Current.Time) == "" {
		return "", fmt.Errorf("forecast for %s: upstream returned no current conditions", place.Name)
	}

	return formatWeather(place, forecast), nil
}

// formatWeather renders the resolved place + current conditions as plain text.
func formatWeather(place geoResult, f forecastResult) string {
	cur := f.Current
	tempUnit := orDefault(f.CurrentUnits.Temperature, "°C")
	windUnit := orDefault(f.CurrentUnits.WindSpeed, "km/h")

	var b strings.Builder
	fmt.Fprintf(&b, "Weather for %s:\n", placeLabel(place))
	fmt.Fprintf(&b, "Conditions: %s\n", weatherCodeText(cur.WeatherCode))
	fmt.Fprintf(&b, "Temperature: %.1f%s (feels like %.1f%s)\n", cur.Temperature, tempUnit, cur.Apparent, tempUnit)
	fmt.Fprintf(&b, "Humidity: %d%%\n", cur.Humidity)
	fmt.Fprintf(&b, "Wind: %.1f %s\n", cur.WindSpeed, windUnit)
	if cur.Time != "" {
		fmt.Fprintf(&b, "Observed (local time): %s", cur.Time)
	}
	return strings.TrimRight(b.String(), "\n")
}

// placeLabel builds "City, Region, Country" skipping empty parts.
func placeLabel(p geoResult) string {
	parts := make([]string, 0, 3)
	for _, s := range []string{p.Name, p.Admin1, p.Country} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// orDefault returns s, or def when s is empty.
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// getJSON performs a context-aware GET and decodes a JSON body. It caps the read
// so a misbehaving (or hostile) upstream can't stream an unbounded body into
// memory during decode, and treats any non-2xx as an error.
func getJSON(ctx context.Context, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := weatherHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned %s", resp.Status)
	}
	// 1 MiB is far more than either Open-Meteo response; it just bounds the blast
	// radius of a runaway upstream.
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst)
}

// weatherCodeText maps a WMO weather interpretation code to a short phrase.
// https://open-meteo.com/en/docs — codes the API can return for `weather_code`.
func weatherCodeText(code int) string {
	switch code {
	case 0:
		return "Clear sky"
	case 1:
		return "Mainly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Overcast"
	case 45, 48:
		return "Fog"
	case 51, 53, 55:
		return "Drizzle"
	case 56, 57:
		return "Freezing drizzle"
	case 61, 63, 65:
		return "Rain"
	case 66, 67:
		return "Freezing rain"
	case 71, 73, 75:
		return "Snowfall"
	case 77:
		return "Snow grains"
	case 80, 81, 82:
		return "Rain showers"
	case 85, 86:
		return "Snow showers"
	case 95:
		return "Thunderstorm"
	case 96, 99:
		return "Thunderstorm with hail"
	default:
		return fmt.Sprintf("Unknown (WMO code %d)", code)
	}
}
