.PHONY: build test dist sim

build:
	go build -o bin/upax-relay ./cmd/upax-relay

test:
	go test -race ./...

dist:
	scripts/build-dist.sh

# run a simulated K14 Pro on 127.0.0.1:4370 for trying the relay without hardware
sim:
	go run ./cmd/zksim
