package main

import (
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	influxapi "github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/joshuabeny1999/netatmo-api-go/v2"
	"log"
	"os"
	"time"
)

// Command line flag
var fConfig = flag.String("f", "netatmo.conf", "Configuration file")
var verbose = flag.Bool("v", false, "verbose output")

// NetatmoConfig API credentials
type NetatmoConfig struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	InfluxUrl    string
	InfluxToken  string
	InfluxOrg    string
	InfluxBucket string
}

var config NetatmoConfig

func main() {
	// Parse command line flags
	flag.Parse()
	if *fConfig == "" {
		fmt.Printf("Missing required argument -f\n")
		os.Exit(0)
	}

	if _, err := toml.DecodeFile(*fConfig, &config); err != nil {
		fmt.Printf("Cannot parse config file: %s\n", err)
		os.Exit(1)
	}

	netatmoConnection, err := authenticate()
	if err != nil {
		log.Fatalln("Error: ", err)
	}

	devices, err := netatmoConnection.Read()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	ct := time.Now().UTC().Unix()

	client := influxdb2.NewClient(config.InfluxUrl, config.InfluxToken)
	writeAPI := client.WriteAPI(config.InfluxOrg, config.InfluxBucket)

	var numPoints = 0
	for _, station := range devices.Stations() {
		for _, module := range station.Modules() {

			if writeModule2Influx(station, module, writeAPI) {
				numPoints++
			}

			if *verbose {
				ts, data := module.Info()
				for dataName, value := range data {
					fmt.Printf("\t%s : %v\t", dataName, value)
				}
				fmt.Printf("\t(updated %ds ago)\n", ct-ts)
				ts, data = module.Data()
				for dataName, value := range data {
					fmt.Printf("\t%s : %v\t", dataName, value)
				}
				fmt.Printf("\t(updated %ds ago)\n", ct-ts)
			}
		}
	}

	log.Printf("write %d points\n", numPoints)

	client.Close()
}

func writeModule2Influx(station *netatmo.Device, module *netatmo.Device, writeAPI influxapi.WriteAPI) bool {
	ts, data := module.Data()
	updateDate := time.Unix(ts, 0)

	fields := make(map[string]interface{})
	for dataName, value := range data {
		fields[dataName] = value
	}

	if len(fields) == 0 || ts == 0 {
		if *verbose {
			fmt.Printf("addPoint(%s / %s): no fields (or o updatedate) ; skip it\n", station.StationName, module.ModuleName)
		}
		return false
	}

	tags := map[string]string{
		"station": station.StationName,
		"module":  module.ModuleName,
	}

	point := influxdb2.NewPoint(
		"netatmo",
		tags,
		fields,
		updateDate,
	)
	writeAPI.WritePoint(point)
	if *verbose {
		fmt.Printf("addPoint(%v)\n", point)
	}

	return true
}

func authenticate() (*netatmo.Client, error) {

	n, err := netatmo.NewClient(netatmo.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		RefreshToken: config.RefreshToken,
	})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	return n, err
}
