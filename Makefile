build:
	go build -o netatmo .

raspi:
	env GOOS=linux GOARCH=arm GOARM=7 go build -o netatmo-pi .
	env GOOS=linux GOARCH=arm64 go build -o netatmo-pi64 .

run:
	go run .

test:
	go test ./...

all: build raspi
