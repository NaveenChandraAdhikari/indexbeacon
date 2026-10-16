BIN     := bin/idxa
PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/indexbeacon/indexbeacon/cmd.version=$(VERSION) -X github.com/indexbeacon/indexbeacon/cmd.commit=$(COMMIT) -X github.com/indexbeacon/indexbeacon/cmd.date=$(DATE)

.PHONY: build test lint test-integration test-e2e vet tidy clean

build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) .

vet:
	go vet $(PKG)

test:
	go test -race -count=1 $(PKG)

test-integration:
	go test -race -count=1 -tags integration -timeout 20m ./internal/... ./cmd/...

test-e2e:
	go test -count=1 -tags e2e -timeout 30m ./e2e/...

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

clean:
	rm -rf bin dist

docker:
	docker build -t idxa:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

docker-run: docker
	docker run --rm -p 8777:8777 -v idxa-data:/var/lib/idxa -e IDXA_TOKEN=local idxa:$(VERSION)

publish:
	./scripts/publish.sh $(NAMESPACE) $(VERSION)

deploy:
	./scripts/deploy.sh --image $(IMAGE)
