.PHONY: build build-mac clean test vet lint

# tgcc — Telegram Forum Topics ↔ Claude Code bridge
# Single binary build using modernc.org/sqlite (CGO_ENABLED=0)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.2.0")
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/tgcc ./cmd/tgcc
	mkdir -p bin/migrations
	cp migrations/*.sql bin/migrations/

build-mac:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/tgcc-mac ./cmd/tgcc
	mkdir -p bin/migrations
	cp migrations/*.sql bin/migrations/

build-mac-arm:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/tgcc-mac-arm64 ./cmd/tgcc
	mkdir -p bin/migrations
	cp migrations/*.sql bin/migrations/

clean:
	rm -rf bin/

test:
	go test -race -cover ./...

vet:
	go vet ./...

lint:
	staticcheck ./...
