GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/crispy-streaming/crispy-companion/internal/server.Version=$(VERSION)

.PHONY: build builddefaults test vet lint clean

# Compile deployment defaults (CRISPY_SERVER_URL / CRISPY_ALLOWED_ORIGINS)
# from the environment or .env — see .env.example.
builddefaults:
	bash scripts/builddefaults.sh

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/crispy-companion ./cmd/crispy-companion

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./... 2>/dev/null || $(GO) vet ./...

clean:
	rm -rf bin/
