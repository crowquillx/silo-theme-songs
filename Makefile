.PHONY: test build build-all notices

test:
	go test -race ./...
	go vet ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/plugin .

notices:
	python3 scripts/third-party-notices.py

build-all:
	python3 scripts/release.py
