.PHONY: build test vet fmt run image pull smoke clean

build:
	go build -o bin/server ./cmd/server

vet:
	go vet ./...

fmt:
	gofmt -l -w .

test:
	go test ./... -timeout 600s

run: build
	./bin/server

# Pull the toolchain image used by the docker runner (first setup only).
pull:
	docker pull golang:1.26-alpine

smoke: build pull
	GSB_LISTEN=:8080 ./bin/server

clean:
	rm -rf bin
