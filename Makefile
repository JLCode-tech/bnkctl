.PHONY: all build build-bnkctl build-awsbnkctl build-azrbnkctl build-gkebnkctl test vet lint tidy clean

VERSION ?= v2.0.0-unified
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/JLCode-tech/bnkctl

LDFLAGS := -s -w \
	-X $(PKG)/internal/cli.Version=$(VERSION)

BINS := bin/bnkctl bin/awsbnkctl bin/azrbnkctl bin/gkebnkctl

all: build

build: $(BINS)

bin/bnkctl: build-bnkctl
build-bnkctl:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/bnkctl ./cmd/bnkctl

bin/awsbnkctl: build-awsbnkctl
build-awsbnkctl:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/awsbnkctl ./cmd/awsbnkctl

bin/azrbnkctl: build-azrbnkctl
build-azrbnkctl:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/azrbnkctl ./cmd/azrbnkctl

bin/gkebnkctl: build-gkebnkctl
build-gkebnkctl:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/gkebnkctl ./cmd/gkebnkctl

test:
	go test ./...

vet:
	go vet ./...

lint:
	@out=$$(gofmt -d -l ./cmd ./internal ./pkg); if [ -n "$$out" ]; then echo "$$out"; echo "gofmt: files need formatting (run 'gofmt -w ./cmd ./internal ./pkg')"; exit 1; fi
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/ dist/ coverage.out
