# Netatmo Weather Data into InfluxDB

Reads all values from your Netatmo weather station into InfluxDB (2.x, or 1.8+
via its v2 write endpoint).
Runs as a one-shot job (for cron) or as a long-running daemon, and keeps its
OAuth2 tokens alive on its own — no manual re-authorization during normal
operation.

## Configuration

Copy `netatmo.conf.dist` to `netatmo.conf` and fill it in:

```toml
clientID      = "NETATMO_CLIENTID"
clientSecret  = "NETATMO_CLIENTSECRET"
RefreshToken  = "NETATMO_REFRESHTOKEN"   # first start only, see below
#TokenFile    = "netatmo-token.json"     # optional, default shown

InfluxUrl     = "INFLUX_URL"
InfluxToken   = "INFLUX_TOKEN"    # InfluxDB 2.x
InfluxOrg     = "INFLUX_ORG"
InfluxBucket  = "INFLUX_BUCKET"
```

### InfluxDB 1.8+

InfluxDB 1.8 and newer serve the v2 write endpoint, so they work too. Set
`InfluxDBName` instead of `InfluxOrg`/`InfluxBucket`:

```toml
InfluxUrl     = "http://192.168.1.10:8086"
InfluxDBName  = "smarthome"
#InfluxRP       = "autogen"   # retention policy, defaults to the database default
#InfluxUser     = ""          # only if the server requires authentication
#InfluxPassword = ""
```

The two modes are mutually exclusive; combining them is rejected at startup.

The config file is only ever read. The rotating tokens are kept in a separate
file (`TokenFile`, mode 0600), written atomically, so a token update can never
damage your InfluxDB settings and a power loss can never leave a half-written
token behind.

## Usage

```
netatmo                        # one-shot, reads netatmo.conf and exits
netatmo -f /etc/netatmo.conf   # different config file
netatmo -interval 10m          # daemon: poll every 10 minutes until stopped
netatmo -v                     # verbose output
netatmo -dry-run               # read from Netatmo, print points, write nothing
```

Netatmo stations push new measurements roughly every 10 minutes, so polling
faster than that gains nothing.

## Getting the first refresh token

Netatmo apps have a token generator on the app page — the quickest way:

1. Open your app at <https://dev.netatmo.com/apps>
2. In the *Token generator* section pick the scope `read_station` and generate
3. Copy the **refresh token** into `RefreshToken` in `netatmo.conf`

Alternatively via the OAuth2 authorization code flow
([docs](https://dev.netatmo.com/apidocumentation/oauth)):

```shell
curl -w "%{redirect_url}" -o /dev/null -s \
  'https://api.netatmo.com/oauth2/authorize?client_id=<your-client-id>&redirect_uri=<your-uri-of-the-app-optional>&scope=read_station&state=<random>'
```

1. Open the URL from the output in a browser
2. Press the button to accept the request
3. Extract the `code` from the result

```shell
curl -d "grant_type=authorization_code&client_id=<your-client-id>&client_secret=<your-secret>&code=<code-from-last-call>&redirect_uri=<your-uri-of-the-app-optional>&scope=read_station" \
  -X POST https://api.netatmo.com/oauth2/token
```

You only need to do this **once**. On first start the program exchanges that
token and stores the result in the token file; from then on it refreshes itself.
`RefreshToken` in the config is ignored as soon as the token file exists.

### Why the token has to be stored

Since 17 April 2023 Netatmo rotates refresh tokens: every call to the token
endpoint issues a new access **and** refresh token and invalidates the previous
pair. A refresh token kept in a static config file therefore works exactly once.
That is why this program owns its token file and rewrites it on every rotation.

Access tokens are valid for 3 hours, and the stored expiry is honoured — a
one-shot run started every 10 minutes only contacts the token endpoint about
once every 3 hours.

If you ever see `invalid_grant`, the stored chain is broken: delete the token
file, put a freshly generated refresh token into the config, and start again.

## Building

```
make build   # native binary
make raspi   # cross-compile: netatmo-pi (armv7) and netatmo-pi64 (arm64)
make test    # run the test suite
```

## Running on a Raspberry Pi

Via cron:

```
*/10 * * * * /home/pi/netatmo/netatmo-pi64 -f /home/pi/netatmo/netatmo.conf
```

Or as a systemd service, which keeps the token cached in memory between polls:

```ini
[Unit]
Description=Netatmo to InfluxDB
After=network-online.target

[Service]
Type=simple
User=pi
WorkingDirectory=/home/pi/netatmo
ExecStart=/home/pi/netatmo/netatmo-pi64 -f /home/pi/netatmo/netatmo.conf -interval 10m
Restart=on-failure
RestartSec=60

[Install]
WantedBy=multi-user.target
```

Note `Restart=on-failure` will not help against a dead refresh token — the
program exits with a clear message in that case, and it needs your attention.

## Data model

One InfluxDB point per module, measurement `netatmo`, tagged with `station` and
`module`. The point timestamp is the Netatmo reading time, not the collection
time. Fields are whatever the module reports: `Temperature`, `Humidity`, `CO2`,
`Noise`, `Pressure`, `AbsolutePressure`, `Rain`, `WindStrength`, and so on.
