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

--

How to get the refresh token from NETATMO

Look into the docs: https://dev.netatmo.com/apidocumentation/oauth#refreshing-a-token

```shell
curl  -w "%{redirect_url}" -o /dev/null -s \
  'https://api.netatmo.com/oauth2/authorize?client_id=<your-client-id>&redirect_uri=<your-uri-of-the-app-optional>&scope=read_station&state=<random>'
```

1. Grap the URL from the output and put it into a browser.
2. Press the button to accept the request.
3. Extract the code from the result

```shell
curl -d "grant_type=authorization_code&client_id=<your-client-id>&client_secret=<your-secret>&code=<code-from-last-call>&redirect_uri=<your-uri-of-the-app-optional>&scope=read_station" \
  -X POST https://api.netatmo.com/oauth2/token
```
