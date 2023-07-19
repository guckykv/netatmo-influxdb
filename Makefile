
build:
	go build -o netatmo netatmo.go

raspi:
	env GOOS=linux GOARCH=arm GOARM=7 go build -o netatmo-pi netatmo.go
	env GOOS=linux GOARCH=arm64 go build -o netatmo-pi64 netatmo.go

run:
	go run netatmo.go

all: build raspi