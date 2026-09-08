package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	influxapi "github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"
	"golang.org/x/oauth2"
)

// Command line flags
var (
	fConfig  = flag.String("f", "netatmo.conf", "Configuration file")
	verbose  = flag.Bool("v", false, "verbose output")
	interval = flag.Duration("interval", 0, "poll continuously at this interval (e.g. 10m); one-shot when unset")
	dryRun   = flag.Bool("dry-run", false, "read from Netatmo and print the points instead of writing to InfluxDB")
)

// NetatmoConfig is the user-edited configuration. It is only ever read, never
// written back: rotating tokens live in a separate file (see TokenFile) so a
// token write can never clobber the InfluxDB settings.
type NetatmoConfig struct {
	ClientID     string
	ClientSecret string
	RefreshToken string // bootstrap only, used when TokenFile does not exist yet
	TokenFile    string // defaults to netatmo-token.json next to the config
	InfluxUrl    string

	// InfluxDB 2.x
	InfluxToken  string
	InfluxOrg    string
	InfluxBucket string

	// InfluxDB 1.8+ compatibility. Setting InfluxDBName selects this mode.
	InfluxDBName   string
	InfluxRP       string // retention policy, defaults to the database default
	InfluxUser     string
	InfluxPassword string
}

var config NetatmoConfig

// placeholderValues are the sample values from netatmo.conf.dist. Leaving one
// in place is a common slip and produces a confusing invalid_client from the
// API, so it is caught before any request goes out.
var placeholderValues = map[string]bool{
	"NETATMO_CLIENTID":     true,
	"NETATMO_CLIENTSECRET": true,
	"NETATMO_REFRESHTOKEN": true,
	"INFLUX_URL":           true,
	"INFLUX_TOKEN":         true,
	"INFLUX_ORG":           true,
	"INFLUX_BUCKET":        true,
}

func main() {
	flag.Parse()
	if *fConfig == "" {
		fmt.Printf("Missing required argument -f\n")
		os.Exit(2)
	}

	if _, err := toml.DecodeFile(*fConfig, &config); err != nil {
		fmt.Printf("Cannot parse config file: %s\n", err)
		os.Exit(1)
	}
	if err := validateConfig(!*dryRun); err != nil {
		fmt.Printf("Invalid configuration: %s\n", err)
		os.Exit(1)
	}

	tokenPath := resolveTokenPath()

	tokenSource, err := buildTokenSource(tokenPath)
	if err != nil {
		log.Fatalln("Error:", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	netatmoClient := NewNetatmoClient(ctx, tokenSource)

	// A nil writeAPI means dry run: everything is read and formatted, nothing
	// is written. Useful to verify credentials before InfluxDB even exists.
	var writeAPI influxapi.WriteAPIBlocking
	if *dryRun {
		log.Println("dry run: not writing to InfluxDB")
	} else {
		token, org, bucket := influxTarget()
		influxClient := influxdb2.NewClient(config.InfluxUrl, token)
		defer influxClient.Close()
		// Blocking API: WritePoint returns the actual write error. The
		// non-blocking WriteAPI only queues and drops errors into a channel,
		// which would report success even while InfluxDB is unreachable.
		writeAPI = influxClient.WriteAPIBlocking(org, bucket)
	}

	if *interval <= 0 {
		if err := collectOnce(ctx, netatmoClient, writeAPI); err != nil {
			log.Printf("Error: %v", err)
			if reason := authFailureReason(err); reason != "" {
				log.Print(reason)
			}
			os.Exit(1)
		}
		return
	}

	runLoop(ctx, netatmoClient, writeAPI)
}

// runLoop polls until interrupted. Transient failures (network hiccup, Influx
// restart) are logged and retried on the next tick; only a permanently dead
// refresh token aborts, because no amount of retrying will fix that.
func runLoop(ctx context.Context, nc *NetatmoClient, writeAPI influxapi.WriteAPIBlocking) {
	log.Printf("polling every %s (station data updates every ~10 minutes)", *interval)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		if err := collectOnce(ctx, nc, writeAPI); err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("collection failed: %v", err)
			if reason := authFailureReason(err); reason != "" {
				log.Print(reason)
				os.Exit(1)
			}
		}

		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-ticker.C:
		}
	}
}

// collectOnce reads all stations and writes one point per module.
func collectOnce(ctx context.Context, nc *NetatmoClient, writeAPI influxapi.WriteAPIBlocking) error {
	devices, err := nc.Read(ctx)
	if err != nil {
		return fmt.Errorf("reading netatmo data: %w", err)
	}

	now := time.Now().UTC().Unix()
	points := make([]*write.Point, 0, 8)

	for _, station := range devices.Stations() {
		for _, module := range station.Modules() {
			if point := buildPoint(station, module); point != nil {
				points = append(points, point)
			}

			if *verbose {
				ts, data := module.Info()
				for dataName, value := range data {
					fmt.Printf("\t%s : %v\t", dataName, value)
				}
				fmt.Printf("\t(updated %ds ago)\n", now-ts)
				ts, data = module.Data()
				for dataName, value := range data {
					fmt.Printf("\t%s : %v\t", dataName, value)
				}
				fmt.Printf("\t(updated %ds ago)\n", now-ts)
			}
		}
	}

	if len(points) == 0 {
		log.Println("no points to write")
		return nil
	}

	// All points go out in a single request.
	if writeAPI == nil {
		for _, point := range points {
			fmt.Print(write.PointToLineProtocol(point, time.Second))
		}
	} else if err := writeAPI.WritePoint(ctx, points...); err != nil {
		return fmt.Errorf("writing to influxdb: %w", err)
	}

	log.Printf("wrote %d points\n", len(points))
	return nil
}

// buildPoint turns one module's readings into an InfluxDB point, or nil if the
// module reported nothing usable.
func buildPoint(station, module *Device) *write.Point {
	ts, data := module.Data()

	if len(data) == 0 || ts == 0 {
		if *verbose {
			fmt.Printf("addPoint(%s / %s): no fields (or no update date) ; skip it\n", station.StationName, module.ModuleName)
		}
		return nil
	}

	tags := map[string]string{
		"station": station.StationName,
		"module":  module.ModuleName,
	}

	point := influxdb2.NewPoint("netatmo", tags, data, time.Unix(ts, 0))
	if *verbose {
		fmt.Printf("addPoint(%v)\n", point)
	}
	return point
}

// buildTokenSource seeds the OAuth2 flow from the token file, falling back to
// the refresh token in the config on first run.
func buildTokenSource(tokenPath string) (oauth2.TokenSource, error) {
	seed, err := loadToken(tokenPath)
	if err != nil {
		return nil, err
	}

	if seed == nil {
		if config.RefreshToken == "" {
			return nil, fmt.Errorf("no token file at %s and no RefreshToken in %s;\n"+
				"generate a token with scope read_station at https://dev.netatmo.com/apps", tokenPath, *fConfig)
		}
		log.Printf("bootstrapping from RefreshToken in %s, storing tokens in %s", *fConfig, tokenPath)
		// Zero expiry forces an immediate refresh, which yields the first
		// access token and the rotated refresh token.
		seed = &oauth2.Token{RefreshToken: config.RefreshToken}
	}

	return newPersistentTokenSource(
		netatmoOAuthConfig(config.ClientID, config.ClientSecret),
		seed,
		tokenPath,
	), nil
}

// resolveTokenPath places the token file next to the config file unless an
// absolute path was configured, so relative paths behave the same whether the
// program is started by cron, systemd, or by hand.
func resolveTokenPath() string {
	path := config.TokenFile
	if path == "" {
		path = "netatmo-token.json"
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(*fConfig), path)
}

// influxTarget maps the configuration onto the arguments the v2 client needs.
// InfluxDB 1.8+ serves the v2 write endpoint, where the org is empty, the
// bucket is "database/retention-policy", and the token is "user:password".
func influxTarget() (token, org, bucket string) {
	if config.InfluxDBName == "" {
		return config.InfluxToken, config.InfluxOrg, config.InfluxBucket
	}

	bucket = config.InfluxDBName
	if config.InfluxRP != "" {
		bucket += "/" + config.InfluxRP
	}
	// An unauthenticated 1.x server wants an empty token, not ":".
	if config.InfluxUser != "" || config.InfluxPassword != "" {
		token = config.InfluxUser + ":" + config.InfluxPassword
	}
	return token, "", bucket
}

func validateConfig(requireInflux bool) error {
	required := map[string]string{
		"clientID":     config.ClientID,
		"clientSecret": config.ClientSecret,
	}
	if requireInflux {
		required["InfluxUrl"] = config.InfluxUrl

		if config.InfluxDBName != "" {
			// 1.x mode: the database name is all that is required, and
			// credentials are optional on an unauthenticated server.
			if config.InfluxBucket != "" || config.InfluxOrg != "" {
				return fmt.Errorf("InfluxDBName (1.x) cannot be combined with InfluxOrg/InfluxBucket (2.x); pick one")
			}
		} else {
			required["InfluxToken"] = config.InfluxToken
			required["InfluxOrg"] = config.InfluxOrg
			required["InfluxBucket"] = config.InfluxBucket
		}
	}

	missing := []string{}
	unedited := []string{}
	for name, value := range required {
		switch {
		case value == "":
			missing = append(missing, name)
		case placeholderValues[value]:
			unedited = append(unedited, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(unedited)

	if len(unedited) > 0 {
		return fmt.Errorf("setting(s) still at the netatmo.conf.dist placeholder: %s",
			strings.Join(unedited, ", "))
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing setting(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// isAuthFailure reports whether the error is an OAuth2 rejection, meaning no
// amount of retrying will help and manual intervention is required.
func isAuthFailure(err error) bool {
	return authFailureReason(err) != ""
}

// authFailureReason returns an operator-facing explanation for an OAuth2
// rejection, or "" if the error is something retryable like a network hiccup.
func authFailureReason(err error) string {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return ""
	}

	switch {
	case re.ErrorCode == "invalid_client":
		return fmt.Sprintf("Netatmo rejected the application credentials: check clientID and clientSecret in %s", *fConfig)
	case re.ErrorCode == "invalid_grant":
		return fmt.Sprintf("the stored refresh token is no longer accepted by Netatmo.\n"+
			"Delete %s, generate a new token with scope read_station at\n"+
			"https://dev.netatmo.com/apps and put it into RefreshToken in %s", resolveTokenPath(), *fConfig)
	case re.Response != nil && (re.Response.StatusCode == http.StatusBadRequest || re.Response.StatusCode == http.StatusUnauthorized):
		return fmt.Sprintf("Netatmo rejected the token request (HTTP %d): %s", re.Response.StatusCode, re.ErrorCode)
	}
	return ""
}
