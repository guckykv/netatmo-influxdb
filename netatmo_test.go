package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"golang.org/x/oauth2"
)

// rotatingTokenServer mimics Netatmo's token endpoint since April 2023: every
// call issues a fresh access token AND a fresh refresh token, and rejects any
// refresh token that is not the most recently issued one.
type rotatingTokenServer struct {
	*httptest.Server
	valid string // the only refresh token currently accepted
	calls int
}

func newRotatingTokenServer(t *testing.T, initial string) *rotatingTokenServer {
	t.Helper()
	rt := &rotatingTokenServer{valid: initial}
	rt.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("cannot parse token request: %v", err)
		}
		rt.calls++

		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", r.PostForm.Get("grant_type"))
		}
		// Netatmo requires the credentials in the body, not as HTTP Basic auth.
		if r.PostForm.Get("client_id") == "" || r.PostForm.Get("client_secret") == "" {
			t.Errorf("client credentials missing from request body: %v", r.PostForm)
		}

		if got := r.PostForm.Get("refresh_token"); got != rt.valid {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}

		rt.valid = fmt.Sprintf("refresh-%d", rt.calls)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":%q,"expires_in":10800,"token_type":"bearer"}`,
			rt.calls, rt.valid)
	}))
	t.Cleanup(rt.Close)
	return rt
}

func testOAuthConfig(tokenURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     "cid",
		ClientSecret: "secret",
		Endpoint:     oauth2.Endpoint{TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
}

func readStoredToken(t *testing.T, path string) storedToken {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}
	var st storedToken
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("parsing token file: %v", err)
	}
	return st
}

// TestRotatedRefreshTokenIsPersisted is the regression test for the original
// bug: the rotated refresh token must reach disk, or the next run is locked out.
func TestRotatedRefreshTokenIsPersisted(t *testing.T) {
	srv := newRotatingTokenServer(t, "bootstrap-token")
	path := filepath.Join(t.TempDir(), "netatmo-token.json")

	seed := &oauth2.Token{RefreshToken: "bootstrap-token"}
	ts := newPersistentTokenSource(testOAuthConfig(srv.URL), seed, path)

	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("first refresh failed: %v", err)
	}
	if tok.AccessToken != "access-1" {
		t.Errorf("access token = %q, want access-1", tok.AccessToken)
	}

	stored := readStoredToken(t, path)
	if stored.RefreshToken != "refresh-1" {
		t.Fatalf("stored refresh token = %q, want the rotated refresh-1", stored.RefreshToken)
	}
	if stored.AccessToken != "access-1" {
		t.Errorf("stored access token = %q, want access-1", stored.AccessToken)
	}
	if stored.Expiry.IsZero() {
		t.Error("stored expiry is zero, so the next run cannot reuse the access token")
	}
}

// TestSurvivesRestartsAcrossRotations simulates repeated cron runs: each run
// starts a fresh process, loads the token file, refreshes, and must still be
// accepted by a server that invalidates every superseded refresh token.
func TestSurvivesRestartsAcrossRotations(t *testing.T) {
	srv := newRotatingTokenServer(t, "bootstrap-token")
	path := filepath.Join(t.TempDir(), "netatmo-token.json")

	seed := &oauth2.Token{RefreshToken: "bootstrap-token"}
	for run := 1; run <= 5; run++ {
		if run > 1 {
			loaded, err := loadToken(path)
			if err != nil {
				t.Fatalf("run %d: loading token: %v", run, err)
			}
			if loaded == nil {
				t.Fatalf("run %d: token file vanished", run)
			}
			// Force a refresh, as would happen after the 3h access token expiry.
			loaded.Expiry = time.Now().Add(-time.Minute)
			seed = loaded
		}

		ts := newPersistentTokenSource(testOAuthConfig(srv.URL), seed, path)
		if _, err := ts.Token(); err != nil {
			t.Fatalf("run %d: refresh rejected: %v", run, err)
		}
	}

	if srv.calls != 5 {
		t.Errorf("token endpoint called %d times, want 5", srv.calls)
	}
}

// TestValidAccessTokenIsNotRefreshed guards the other half of the fix: a run
// that starts with an unexpired access token must not touch the token endpoint,
// because every needless refresh burns the stored refresh token.
func TestValidAccessTokenIsNotRefreshed(t *testing.T) {
	srv := newRotatingTokenServer(t, "stored-token")
	path := filepath.Join(t.TempDir(), "netatmo-token.json")

	seed := &oauth2.Token{
		AccessToken:  "still-good",
		RefreshToken: "stored-token",
		Expiry:       time.Now().Add(2 * time.Hour),
	}
	ts := newPersistentTokenSource(testOAuthConfig(srv.URL), seed, path)

	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token() failed: %v", err)
	}
	if tok.AccessToken != "still-good" {
		t.Errorf("access token = %q, want the cached still-good", tok.AccessToken)
	}
	if srv.calls != 0 {
		t.Errorf("token endpoint called %d times, want 0", srv.calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("token file was rewritten even though nothing rotated")
	}
}

// TestDeadRefreshTokenIsRecognized checks that an exhausted token surfaces as
// an auth failure, so the daemon aborts instead of looping forever.
func TestDeadRefreshTokenIsRecognized(t *testing.T) {
	srv := newRotatingTokenServer(t, "the-only-valid-one")
	path := filepath.Join(t.TempDir(), "netatmo-token.json")

	seed := &oauth2.Token{RefreshToken: "a-stale-token"}
	ts := newPersistentTokenSource(testOAuthConfig(srv.URL), seed, path)

	_, err := ts.Token()
	if err == nil {
		t.Fatal("expected the stale refresh token to be rejected")
	}
	if !isAuthFailure(err) {
		t.Errorf("isAuthFailure(%v) = false, want true", err)
	}
}

func TestSaveTokenRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "netatmo-token.json")
	want := &oauth2.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		Expiry:       time.Now().Add(time.Hour).Round(time.Second),
	}

	if err := saveToken(path, want); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	got, err := loadToken(path)
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if !got.Expiry.Equal(want.Expiry) {
		t.Errorf("expiry = %v, want %v", got.Expiry, want.Expiry)
	}
}

func TestSaveTokenLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "netatmo-token.json")

	for i := 0; i < 3; i++ {
		if err := saveToken(path, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
			t.Fatalf("saveToken: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want only the token file: %v", len(entries), entries)
	}
}

func TestLoadTokenMissingFileIsNotAnError(t *testing.T) {
	got, err := loadToken(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Errorf("loadToken on missing file returned %v, want nil", err)
	}
	if got != nil {
		t.Errorf("loadToken on missing file returned %+v, want nil", got)
	}
}

// stationsResponse is a trimmed getstationsdata payload with three modules:
// a station, a healthy outdoor module, and a module that has never reported
// (no time_utc, no readings) — the shape that used to panic.
const stationsResponse = `{
  "body": {
    "devices": [{
      "_id": "70:ee:00:00:00:01",
      "station_name": "Zuhause",
      "module_name": "Innen",
      "type": "NAMain",
      "wifi_status": 56,
      "dashboard_data": {
        "Temperature": 21.5, "Humidity": 47, "CO2": 620,
        "Noise": 35, "Pressure": 1013.2, "AbsolutePressure": 964.1,
        "temp_trend": "stable", "time_utc": 1757320000
      },
      "modules": [{
        "_id": "02:00:00:00:00:02",
        "module_name": "Aussen",
        "type": "NAModule1",
        "battery_percent": 78,
        "rf_status": 70,
        "dashboard_data": {
          "Temperature": 8.3, "Humidity": 81, "min_temp": 6.1,
          "max_temp": 12.4, "time_utc": 1757319900
        }
      }, {
        "_id": "05:00:00:00:00:03",
        "module_name": "Regenmesser",
        "type": "NAModule3",
        "battery_percent": 4,
        "dashboard_data": {}
      }]
    }]
  }
}`

func newStubNetatmo(t *testing.T, body string, status int) *NetatmoClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("app_type"); got != "app_station" {
			t.Errorf("app_type = %q, want app_station", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-access" {
			t.Errorf("Authorization = %q, want the bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	old := netatmoDeviceURL
	netatmoDeviceURL = srv.URL
	t.Cleanup(func() { netatmoDeviceURL = old })

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-access", TokenType: "Bearer"})
	return NewNetatmoClient(context.Background(), ts)
}

func TestReadParsesStationsAndModules(t *testing.T) {
	nc := newStubNetatmo(t, stationsResponse, http.StatusOK)

	dc, err := nc.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	stations := dc.Stations()
	if len(stations) != 1 {
		t.Fatalf("got %d stations, want 1", len(stations))
	}
	if stations[0].StationName != "Zuhause" {
		t.Errorf("station name = %q, want Zuhause", stations[0].StationName)
	}

	// Modules() returns the linked modules plus the station itself.
	modules := stations[0].Modules()
	if len(modules) != 3 {
		t.Fatalf("got %d modules, want 3", len(modules))
	}

	byName := map[string]*Device{}
	for _, m := range modules {
		byName[m.ModuleName] = m
	}

	ts, data := byName["Aussen"].Data()
	if ts != 1757319900 {
		t.Errorf("outdoor timestamp = %d, want 1757319900", ts)
	}
	if data["Temperature"] != float32(8.3) {
		t.Errorf("outdoor temperature = %v, want 8.3", data["Temperature"])
	}
	if data["MinTemp"] != float32(6.1) || data["MaxTemp"] != float32(12.4) {
		t.Errorf("min/max temp = %v/%v, want 6.1/12.4", data["MinTemp"], data["MaxTemp"])
	}
	if _, present := data["CO2"]; present {
		t.Error("outdoor module reported CO2, which it does not have")
	}

	_, info := byName["Aussen"].Info()
	if info["BatteryPercent"] != int32(78) {
		t.Errorf("battery = %v, want 78", info["BatteryPercent"])
	}

	_, stationData := byName["Innen"].Data()
	if stationData["CO2"] != int32(620) || stationData["TempTrend"] != "stable" {
		t.Errorf("station data = %v", stationData)
	}
}

// TestModuleWithoutReadingsDoesNotPanic covers the nil time_utc case that the
// upstream library dereferences blindly.
func TestModuleWithoutReadingsDoesNotPanic(t *testing.T) {
	nc := newStubNetatmo(t, stationsResponse, http.StatusOK)

	dc, err := nc.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var broken *Device
	for _, m := range dc.Stations()[0].Modules() {
		if m.ModuleName == "Regenmesser" {
			broken = m
		}
	}
	if broken == nil {
		t.Fatal("test fixture lost the module without readings")
	}

	ts, data := broken.Data() // must not panic
	if ts != 0 {
		t.Errorf("timestamp = %d, want 0 for a module that never reported", ts)
	}
	if len(data) != 0 {
		t.Errorf("data = %v, want empty", data)
	}

	// Info() carries the battery level even without a dashboard timestamp.
	ts, info := broken.Info()
	if ts != 0 {
		t.Errorf("info timestamp = %d, want 0", ts)
	}
	if info["BatteryPercent"] != int32(4) {
		t.Errorf("battery = %v, want 4", info["BatteryPercent"])
	}
}

func TestReadSurfacesAPIErrorMessage(t *testing.T) {
	nc := newStubNetatmo(t, `{"error":{"code":3,"message":"Access token expired"}}`, http.StatusForbidden)

	_, err := nc.Read(context.Background())
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if !strings.Contains(err.Error(), "Access token expired") {
		t.Errorf("error = %v, want it to include the API message", err)
	}
}

func TestResolveTokenPath(t *testing.T) {
	tests := []struct {
		name       string
		configPath string
		tokenFile  string
		want       string
	}{
		{"default next to config", "/etc/netatmo/netatmo.conf", "", "/etc/netatmo/netatmo-token.json"},
		{"relative resolves against config dir", "/etc/netatmo/netatmo.conf", "tok.json", "/etc/netatmo/tok.json"},
		{"absolute is used verbatim", "/etc/netatmo/netatmo.conf", "/var/lib/netatmo/tok.json", "/var/lib/netatmo/tok.json"},
		{"bare config name stays local", "netatmo.conf", "", "netatmo-token.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldConfig, oldFile := *fConfig, config.TokenFile
			defer func() { *fConfig, config.TokenFile = oldConfig, oldFile }()

			*fConfig, config.TokenFile = tt.configPath, tt.tokenFile
			if got := resolveTokenPath(); got != tt.want {
				t.Errorf("resolveTokenPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateConfigReportsMissingSettings(t *testing.T) {
	old := config
	defer func() { config = old }()

	config = NetatmoConfig{ClientID: "cid", InfluxUrl: "http://localhost:8086"}
	err := validateConfig(true)
	if err == nil {
		t.Fatal("expected missing settings to be reported")
	}
	for _, want := range []string{"clientSecret", "InfluxToken", "InfluxOrg", "InfluxBucket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "clientID") {
		t.Errorf("error %q wrongly flags clientID", err)
	}
}

func TestAuthFailureReasonDistinguishesCauses(t *testing.T) {
	oldConfig := *fConfig
	defer func() { *fConfig = oldConfig }()
	*fConfig = "netatmo.conf"

	tests := []struct {
		name      string
		errorCode string
		status    int
		wantMatch string
	}{
		{"dead refresh token", "invalid_grant", http.StatusBadRequest, "refresh token is no longer accepted"},
		{"wrong credentials", "invalid_client", http.StatusBadRequest, "application credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &oauth2.RetrieveError{
				Response:  &http.Response{StatusCode: tt.status},
				ErrorCode: tt.errorCode,
			}
			got := authFailureReason(err)
			if !strings.Contains(got, tt.wantMatch) {
				t.Errorf("authFailureReason() = %q, want it to mention %q", got, tt.wantMatch)
			}
			if !isAuthFailure(err) {
				t.Error("isAuthFailure() = false, want true")
			}
		})
	}
}

// A network error must not be mistaken for an auth failure, otherwise the
// daemon would exit on a temporary outage instead of retrying.
func TestNetworkErrorIsNotAnAuthFailure(t *testing.T) {
	err := fmt.Errorf("reading netatmo data: dial tcp: connection refused")
	if isAuthFailure(err) {
		t.Error("a network error was classified as an auth failure")
	}
	if got := authFailureReason(err); got != "" {
		t.Errorf("authFailureReason() = %q, want empty", got)
	}
}

// A dry run needs no InfluxDB settings, so they must not be demanded.
func TestValidateConfigSkipsInfluxForDryRun(t *testing.T) {
	old := config
	defer func() { config = old }()

	config = NetatmoConfig{ClientID: "cid", ClientSecret: "secret"}
	if err := validateConfig(false); err != nil {
		t.Errorf("validateConfig(false) = %v, want nil", err)
	}
	if err := validateConfig(true); err == nil {
		t.Error("validateConfig(true) accepted a config without InfluxDB settings")
	}
}

// Leaving a template value in place produces a confusing invalid_client from
// the API, so it must be caught locally.
func TestValidateConfigRejectsPlaceholders(t *testing.T) {
	old := config
	defer func() { config = old }()

	config = NetatmoConfig{ClientID: "NETATMO_CLIENTID", ClientSecret: "NETATMO_CLIENTSECRET"}
	err := validateConfig(false)
	if err == nil {
		t.Fatal("placeholder values were accepted")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error = %q, want it to name the problem", err)
	}
	for _, want := range []string{"clientID", "clientSecret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// A real value alongside a placeholder must still be flagged.
	config = NetatmoConfig{ClientID: "real-id", ClientSecret: "NETATMO_CLIENTSECRET"}
	err = validateConfig(false)
	if err == nil || strings.Contains(err.Error(), "clientID") {
		t.Errorf("error = %v, want only clientSecret flagged", err)
	}
}

func TestInfluxTargetV1AndV2(t *testing.T) {
	old := config
	defer func() { config = old }()

	tests := []struct {
		name                           string
		cfg                            NetatmoConfig
		wantToken, wantOrg, wantBucket string
	}{
		{
			name:       "2.x uses org and bucket",
			cfg:        NetatmoConfig{InfluxToken: "tok", InfluxOrg: "org", InfluxBucket: "buck"},
			wantToken:  "tok",
			wantOrg:    "org",
			wantBucket: "buck",
		},
		{
			name:       "1.x without auth",
			cfg:        NetatmoConfig{InfluxDBName: "smarthome"},
			wantToken:  "",
			wantOrg:    "",
			wantBucket: "smarthome",
		},
		{
			name:       "1.x with retention policy",
			cfg:        NetatmoConfig{InfluxDBName: "smarthome", InfluxRP: "autogen"},
			wantBucket: "smarthome/autogen",
		},
		{
			name:       "1.x with credentials",
			cfg:        NetatmoConfig{InfluxDBName: "smarthome", InfluxUser: "u", InfluxPassword: "p"},
			wantToken:  "u:p",
			wantBucket: "smarthome",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config = tt.cfg
			token, org, bucket := influxTarget()
			if token != tt.wantToken || org != tt.wantOrg || bucket != tt.wantBucket {
				t.Errorf("influxTarget() = (%q, %q, %q), want (%q, %q, %q)",
					token, org, bucket, tt.wantToken, tt.wantOrg, tt.wantBucket)
			}
		})
	}
}

func TestValidateConfigInfluxModes(t *testing.T) {
	old := config
	defer func() { config = old }()

	base := NetatmoConfig{ClientID: "cid", ClientSecret: "sec", InfluxUrl: "http://h:8086"}

	t.Run("1.x needs only the database name", func(t *testing.T) {
		config = base
		config.InfluxDBName = "smarthome"
		if err := validateConfig(true); err != nil {
			t.Errorf("validateConfig = %v, want nil", err)
		}
	})

	t.Run("mixing 1.x and 2.x is rejected", func(t *testing.T) {
		config = base
		config.InfluxDBName = "smarthome"
		config.InfluxBucket = "buck"
		err := validateConfig(true)
		if err == nil || !strings.Contains(err.Error(), "pick one") {
			t.Errorf("validateConfig = %v, want a conflict error", err)
		}
	})

	t.Run("2.x still demands org bucket token", func(t *testing.T) {
		config = base
		err := validateConfig(true)
		if err == nil {
			t.Fatal("expected missing 2.x settings to be reported")
		}
		for _, want := range []string{"InfluxOrg", "InfluxBucket", "InfluxToken"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

func TestBuildPointSkipsModulesWithoutData(t *testing.T) {
	nc := newStubNetatmo(t, stationsResponse, http.StatusOK)
	dc, err := nc.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	station := dc.Stations()[0]

	points := 0
	for _, m := range station.Modules() {
		p := buildPoint(station, m)
		if m.ModuleName == "Regenmesser" {
			if p != nil {
				t.Error("module without readings produced a point")
			}
			continue
		}
		if p == nil {
			t.Fatalf("module %q produced no point", m.ModuleName)
		}
		if p.Name() != "netatmo" {
			t.Errorf("measurement = %q, want netatmo", p.Name())
		}
		points++
	}
	if points != 2 {
		t.Errorf("built %d points, want 2", points)
	}
}

// TestCollectOnceSendsASingleWriteRequest pins the batching behaviour: all
// modules of a run must leave in one HTTP request, not one request per point.
func TestCollectOnceSendsASingleWriteRequest(t *testing.T) {
	nc := newStubNetatmo(t, stationsResponse, http.StatusOK)

	var requests int
	var bodies []string
	influxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests++
		bodies = append(bodies, string(body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer influxSrv.Close()

	client := influxdb2.NewClient(influxSrv.URL, "token")
	defer client.Close()

	if err := collectOnce(context.Background(), nc, client.WriteAPIBlocking("org", "bucket")); err != nil {
		t.Fatalf("collectOnce: %v", err)
	}

	if requests != 1 {
		t.Errorf("InfluxDB received %d requests, want 1", requests)
	}
	// Two writable modules; the third has no readings and is skipped.
	if lines := strings.Count(strings.TrimSpace(bodies[0]), "\n") + 1; lines != 2 {
		t.Errorf("request carried %d lines, want 2:\n%s", lines, bodies[0])
	}
}
