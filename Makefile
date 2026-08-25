APP_NAME    := digitalocean_exporter
PKG         := github.com/kozaktomas/digitalocean_exporter
VERSION     ?= dev
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS     := -s -w -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.Commit=$(COMMIT)

.PHONY: build run fmt vet lint test test-race check smoke docker snapshot chart-lint clean

## build: Compile the exporter binary.
build:
	go build -ldflags "$(LDFLAGS)" -o $(APP_NAME) ./cmd/$(APP_NAME)

## run: Build and run the exporter with the local environment.
run: build
	./$(APP_NAME)

## fmt: Format all Go source files.
fmt:
	gofmt -w .

## vet: Run go vet on all packages.
vet:
	go vet ./...

## lint: Run golangci-lint with the strict configuration.
lint:
	golangci-lint run

## test: Run all tests with coverage.
test:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

## test-race: Run all tests under the race detector.
test-race:
	CGO_ENABLED=1 go test -race ./...

## check: The full quality gate: fmt, vet, lint, test, race detector.
check: fmt vet lint test test-race

## smoke: Run the end-to-end smoke test against a locally built binary.
smoke: build
	./scripts/smoke.sh

## docker: Build the container image for both release architectures.
docker:
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT_SHA=$(COMMIT) -t $(APP_NAME):local .

## snapshot: Dry-run the release pipeline (binaries, deb, tarball).
snapshot:
	goreleaser release --snapshot --clean

## chart-lint: Lint and render the Helm chart.
chart-lint:
	helm lint charts/digitalocean-exporter --set digitalocean.token=dummy
	helm template charts/digitalocean-exporter --set digitalocean.token=dummy >/dev/null

## clean: Remove build artifacts.
clean:
	rm -rf $(APP_NAME) coverage.out dist/
