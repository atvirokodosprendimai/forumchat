package mcpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedNow is a deterministic instant (Sat 2026-06-27 22:30:00 UTC) so the
// formatting assertions don't depend on the wall clock.
var fixedNow = time.Date(2026, time.June, 27, 22, 30, 0, 0, time.UTC)

func TestFormatDateTimeUTCDefault(t *testing.T) {
	out, err := formatDateTime(fixedNow, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Spot-check the load-bearing pieces rather than the whole string.
	if !strings.Contains(out, "2026-06-27 22:30:00") {
		t.Errorf("missing UTC datetime in %q", out)
	}
	if !strings.Contains(out, "Timezone: UTC") {
		t.Errorf("expected UTC default in %q", out)
	}
	if !strings.Contains(out, "Day of week: Saturday") {
		t.Errorf("expected Saturday in %q", out)
	}
}

func TestFormatDateTimeNamedZone(t *testing.T) {
	// Vilnius is UTC+3 in June (DST), so 22:30 UTC → 01:30 the next day.
	out, err := formatDateTime(fixedNow, "Europe/Vilnius")
	if err != nil {
		t.Fatalf("LoadLocation failed — is time/tzdata embedded? err=%v", err)
	}
	if !strings.Contains(out, "2026-06-28 01:30:00") {
		t.Errorf("expected Vilnius local time in %q", out)
	}
	if !strings.Contains(out, "Timezone: Europe/Vilnius") {
		t.Errorf("expected named timezone in %q", out)
	}
}

func TestFormatDateTimeBadZone(t *testing.T) {
	if _, err := formatDateTime(fixedNow, "Mars/Olympus_Mons"); err == nil {
		t.Fatal("expected error for unknown timezone, got nil")
	}
}

// stubWeather installs an httptest server standing in for both Open-Meteo
// endpoints and points the package URLs at it for the duration of the test.
func stubWeather(t *testing.T, geo, forecast string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "search") {
			_, _ = w.Write([]byte(geo))
			return
		}
		_, _ = w.Write([]byte(forecast))
	}))
	t.Cleanup(srv.Close)

	geoOrig, fcOrig := geocodeURL, forecastURL
	geocodeURL = srv.URL + "/v1/search"
	forecastURL = srv.URL + "/v1/forecast"
	t.Cleanup(func() { geocodeURL, forecastURL = geoOrig, fcOrig })
}

func TestFetchWeatherHappyPath(t *testing.T) {
	stubWeather(t,
		`{"results":[{"name":"Vilnius","latitude":54.69,"longitude":25.28,"country":"Lithuania","admin1":"Vilnius"}]}`,
		`{"current":{"time":"2026-06-28T01:30","temperature_2m":17.4,"apparent_temperature":16.2,"relative_humidity_2m":72,"weather_code":3,"wind_speed_10m":11.5},"current_units":{"temperature_2m":"°C","wind_speed_10m":"km/h"}}`,
	)

	out, err := fetchWeather(context.Background(), "Vilnius")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"Vilnius, Lithuania",
		"Overcast", // weather_code 3
		"17.4°C",   // temperature
		"feels like 16.2°C",
		"Humidity: 72%",
		"11.5 km/h",
		"2026-06-28T01:30",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("weather output missing %q\n got: %s", want, out)
		}
	}
}

func TestFetchWeatherNotFound(t *testing.T) {
	// Empty results = no geocoding match. This is a normal "not found", surfaced
	// as a plain message (nil error) so the tool relays it to the model.
	stubWeather(t, `{"results":[]}`, `{}`)

	out, err := fetchWeather(context.Background(), "Nowheresville")
	if err != nil {
		t.Fatalf("expected nil error for no match, got %v", err)
	}
	if !strings.Contains(out, "No place matching") {
		t.Errorf("expected not-found message, got %q", out)
	}
}

func TestFetchWeatherNegativeTemp(t *testing.T) {
	// Sub-zero temps must render with the sign, not get mangled.
	stubWeather(t,
		`{"results":[{"name":"Oymyakon","latitude":63.46,"longitude":142.79,"country":"Russia","admin1":"Sakha"}]}`,
		`{"current":{"time":"2026-01-15T08:00","temperature_2m":-48.3,"apparent_temperature":-55.1,"relative_humidity_2m":80,"weather_code":71,"wind_speed_10m":4.2},"current_units":{"temperature_2m":"°C","wind_speed_10m":"km/h"}}`,
	)
	out, err := fetchWeather(context.Background(), "Oymyakon")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"-48.3°C", "feels like -55.1°C", "Snowfall"} {
		if !strings.Contains(out, want) {
			t.Errorf("weather output missing %q\n got: %s", want, out)
		}
	}
}

func TestFetchWeatherEmptyForecast(t *testing.T) {
	// Geocode succeeds but the forecast body has no current block (HTTP 200).
	// Must error, not fabricate zero-value "Clear sky, 0°C" weather.
	stubWeather(t,
		`{"results":[{"name":"Vilnius","latitude":54.69,"longitude":25.28,"country":"Lithuania"}]}`,
		`{}`,
	)
	if _, err := fetchWeather(context.Background(), "Vilnius"); err == nil {
		t.Fatal("expected error for forecast with no current conditions, got nil")
	}
}

func TestFetchWeatherMalformedForecast(t *testing.T) {
	// Truncated/invalid JSON in the forecast body must surface as an error.
	stubWeather(t,
		`{"results":[{"name":"Vilnius","latitude":54.69,"longitude":25.28,"country":"Lithuania"}]}`,
		`{"current":{"temperature_2m":`,
	)
	if _, err := fetchWeather(context.Background(), "Vilnius"); err == nil {
		t.Fatal("expected error for malformed forecast JSON, got nil")
	}
}

func TestFetchWeatherEmptyLocation(t *testing.T) {
	if _, err := fetchWeather(context.Background(), "   "); err == nil {
		t.Fatal("expected error for blank location, got nil")
	}
}

func TestFetchWeatherUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	geoOrig, fcOrig := geocodeURL, forecastURL
	geocodeURL = srv.URL + "/v1/search"
	forecastURL = srv.URL + "/v1/forecast"
	t.Cleanup(func() { geocodeURL, forecastURL = geoOrig, fcOrig })

	if _, err := fetchWeather(context.Background(), "Vilnius"); err == nil {
		t.Fatal("expected error on 500 upstream, got nil")
	}
}

func TestWeatherCodeText(t *testing.T) {
	cases := map[int]string{0: "Clear sky", 3: "Overcast", 61: "Rain", 95: "Thunderstorm"}
	for code, want := range cases {
		if got := weatherCodeText(code); got != want {
			t.Errorf("weatherCodeText(%d) = %q, want %q", code, got, want)
		}
	}
	if !strings.Contains(weatherCodeText(1234), "Unknown") {
		t.Error("expected unknown-code fallback")
	}
}
