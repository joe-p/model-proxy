GO ?= go
BINARY ?= bin/model-proxy
VERSION ?= dev
PKG := ./cmd/model-proxy

.PHONY: build test vet fmt check integration docker clean

build:
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PKG)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

check:
	@files="$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"; \
	if [ -n "$$files" ]; then \
		echo "gofmt needed:"; \
		echo "$$files"; \
		exit 1; \
	fi
	$(GO) vet ./...
	$(GO) test ./...

integration:
	$(GO) test -tags=integration ./internal/provider/openrouter

docker:
	docker build --build-arg VERSION=$(VERSION) -t model-proxy:$(VERSION) .

clean:
	rm -rf bin
