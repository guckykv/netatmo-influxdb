package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
)

const (
	netatmoBaseURL = "https://api.netatmo.com/"
	netatmoAuthURL = netatmoBaseURL + "oauth2/token"
)

// netatmoDeviceURL is a variable so tests can point it at a stub server.
var netatmoDeviceURL = netatmoBaseURL + "api/getstationsdata"

// netatmoOAuthConfig builds the OAuth2 config for the Netatmo token endpoint.
// AuthStyleInParams is set explicitly: Netatmo expects client_id/client_secret
// in the POST body, and without this x/oauth2 wastes a failing probe request
// with HTTP Basic auth on every fresh process.
func netatmoOAuthConfig(clientID, clientSecret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{"read_station"},
		Endpoint: oauth2.Endpoint{
			TokenURL:  netatmoAuthURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// NetatmoClient reads weather station data from the Netatmo API.
type NetatmoClient struct {
	httpClient *http.Client
}

func NewNetatmoClient(ctx context.Context, ts oauth2.TokenSource) *NetatmoClient {
	return &NetatmoClient{httpClient: oauth2.NewClient(ctx, ts)}
}

// DeviceCollection is the getstationsdata response body.
type DeviceCollection struct {
	Body struct {
		Devices []*Device `json:"devices"`
	} `json:"body"`
}

// Device is a station or one of its modules.
type Device struct {
	ID             string        `json:"_id"`
	StationName    string        `json:"station_name"`
	ModuleName     string        `json:"module_name"`
	Type           string        `json:"type"`
	BatteryPercent *int32        `json:"battery_percent,omitempty"`
	WifiStatus     *int32        `json:"wifi_status,omitempty"`
	RFStatus       *int32        `json:"rf_status,omitempty"`
	DashboardData  DashboardData `json:"dashboard_data"`
	LinkedModules  []*Device     `json:"modules"`
}

// DashboardData holds the sensor measurements. Every field is a pointer so a
// missing value is distinguishable from a zero reading.
type DashboardData struct {
	Temperature      *float32 `json:"Temperature,omitempty"`
	MaxTemp          *float32 `json:"max_temp,omitempty"`
	MinTemp          *float32 `json:"min_temp,omitempty"`
	TempTrend        string   `json:"temp_trend,omitempty"`
	Humidity         *int32   `json:"Humidity,omitempty"`
	CO2              *int32   `json:"CO2,omitempty"`
	Noise            *int32   `json:"Noise,omitempty"`
	Pressure         *float32 `json:"Pressure,omitempty"`
	AbsolutePressure *float32 `json:"AbsolutePressure,omitempty"`
	PressureTrend    string   `json:"pressure_trend,omitempty"`
	Rain             *float32 `json:"Rain,omitempty"`
	Rain1Hour        *float32 `json:"sum_rain_1,omitempty"`
	Rain1Day         *float32 `json:"sum_rain_24,omitempty"`
	WindAngle        *int32   `json:"WindAngle,omitempty"`
	WindStrength     *int32   `json:"WindStrength,omitempty"`
	GustAngle        *int32   `json:"GustAngle,omitempty"`
	GustStrength     *int32   `json:"GustStrength,omitempty"`
	LastMeasure      *int64   `json:"time_utc"`
}

// apiError is the error shape Netatmo returns alongside a non-200 status.
type apiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Read fetches all stations and modules in one API call.
func (c *NetatmoClient) Read(ctx context.Context) (*DeviceCollection, error) {
	params := url.Values{"app_type": {"app_station"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, netatmoDeviceURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var ae apiError
		if json.Unmarshal(body, &ae) == nil && ae.Error.Message != "" {
			return nil, fmt.Errorf("netatmo API returned %d: %s (code %d)", resp.StatusCode, ae.Error.Message, ae.Error.Code)
		}
		return nil, fmt.Errorf("netatmo API returned %d: %s", resp.StatusCode, string(body))
	}

	dc := &DeviceCollection{}
	if err := json.NewDecoder(resp.Body).Decode(dc); err != nil {
		return nil, fmt.Errorf("cannot decode station data: %w", err)
	}
	return dc, nil
}

// Stations returns all base stations in the account.
func (dc *DeviceCollection) Stations() []*Device {
	return dc.Body.Devices
}

// Modules returns the station's linked modules plus the station itself, which
// carries its own set of readings.
func (d *Device) Modules() []*Device {
	list := make([]*Device, 0, len(d.LinkedModules)+1)
	list = append(list, d.LinkedModules...)
	return append(list, d)
}

// Data returns the reading timestamp and the populated sensor values. A module
// that never reported (flat battery, lost radio link) has no time_utc; it
// yields a zero timestamp rather than panicking.
func (d *Device) Data() (int64, map[string]interface{}) {
	m := make(map[string]interface{})
	dd := &d.DashboardData

	if dd.Temperature != nil {
		m["Temperature"] = *dd.Temperature
	}
	if dd.MinTemp != nil {
		m["MinTemp"] = *dd.MinTemp
	}
	if dd.MaxTemp != nil {
		m["MaxTemp"] = *dd.MaxTemp
	}
	if dd.TempTrend != "" {
		m["TempTrend"] = dd.TempTrend
	}
	if dd.Humidity != nil {
		m["Humidity"] = *dd.Humidity
	}
	if dd.CO2 != nil {
		m["CO2"] = *dd.CO2
	}
	if dd.Noise != nil {
		m["Noise"] = *dd.Noise
	}
	if dd.Pressure != nil {
		m["Pressure"] = *dd.Pressure
	}
	if dd.AbsolutePressure != nil {
		m["AbsolutePressure"] = *dd.AbsolutePressure
	}
	if dd.PressureTrend != "" {
		m["PressureTrend"] = dd.PressureTrend
	}
	if dd.Rain != nil {
		m["Rain"] = *dd.Rain
	}
	if dd.Rain1Hour != nil {
		m["Rain1Hour"] = *dd.Rain1Hour
	}
	if dd.Rain1Day != nil {
		m["Rain1Day"] = *dd.Rain1Day
	}
	if dd.WindAngle != nil {
		m["WindAngle"] = *dd.WindAngle
	}
	if dd.WindStrength != nil {
		m["WindStrength"] = *dd.WindStrength
	}
	if dd.GustAngle != nil {
		m["GustAngle"] = *dd.GustAngle
	}
	if dd.GustStrength != nil {
		m["GustStrength"] = *dd.GustStrength
	}

	return dd.lastMeasure(), m
}

// Info returns the reading timestamp and the module status values.
func (d *Device) Info() (int64, map[string]interface{}) {
	m := make(map[string]interface{})

	if d.BatteryPercent != nil {
		m["BatteryPercent"] = *d.BatteryPercent
	}
	if d.WifiStatus != nil {
		m["WifiStatus"] = *d.WifiStatus
	}
	if d.RFStatus != nil {
		m["RFStatus"] = *d.RFStatus
	}

	return d.DashboardData.lastMeasure(), m
}

func (dd *DashboardData) lastMeasure() int64 {
	if dd.LastMeasure == nil {
		return 0
	}
	return *dd.LastMeasure
}
