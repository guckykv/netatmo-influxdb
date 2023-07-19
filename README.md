# Netatmo Weather Data into InfluxDB

This small command read all values from your netatmo account into your InfluxDB v2.x.

Configure your Netatmo Account and Influx URL into `netatmo.conf`.

```
clientID      = "NETATMO_CLIENTID"
clientSecret  = "NETATMO_CLIENTSECRET"
RefreshToken  = "NETATMO_REFRESHTOKEN"
InfluxUrl     = "INFLUX_URL"
InfluxToken   = "INFLUX_TOKEN"
InfluxOrg     = "INFLUX_ORG"
InfluxBucket  = "INFLUX_BUCKET"
```

With the `-f` switch, you can change the config name and oath.
With `-v` you can activate some debug output.

This script uses https://github.com/joshuabeny1999/netatmo-api-go for 
accessing the netatmo api.

