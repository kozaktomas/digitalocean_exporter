APP_NAME    := digitalocean_exporter
PKG         := github.com/kozaktomas/digitalocean_exporter
VERSION     ?= dev
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS     := -s -w -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.Commit=$(COMMIT)

.PHONY: build run fmt fmt-check vet lint test test-race check smoke docker snapshot alerts-lint chart-lint chart-docs docs docs-serve clean

## build: Compile the exporter binary.
build:
	go build -ldflags "$(LDFLAGS)" -o $(APP_NAME) ./cmd/$(APP_NAME)

## run: Build and run the exporter with the local environment.
run: build
	./$(APP_NAME)

## fmt: Format all Go source files in place.
fmt:
	gofmt -w .

## fmt-check: Report unformatted Go source files and fail, changing nothing.
fmt-check:
# The gate must not rewrite the tree: `gofmt -w` in CI formats the workspace, every
# later step then sees clean files, and the job goes green over code that was never
# formatted in the repository. `gofmt -l` only reports, so the failure is visible.
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted (run 'make fmt'):" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

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

## check: The full quality gate: formatting, vet, lint, test, race detector.
check: fmt-check vet lint test test-race

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

## alerts-lint: Check the bundled alerting rules with promtool.
alerts-lint:
	promtool check rules charts/digitalocean-exporter/alerts/*.yaml

## chart-lint: Lint and render the Helm chart in every configuration that has a branch.
chart-lint:
# --strict, so a warning about the chart metadata fails here rather than surfacing
# on somebody's `helm install`.
	helm lint charts/digitalocean-exporter --strict --set digitalocean.token=dummy
	helm template charts/digitalocean-exporter --set digitalocean.token=dummy >/dev/null
# The dashboards are off by default, so the branch that renders them would never
# be exercised without a second run that switches them on.
	helm template charts/digitalocean-exporter --set digitalocean.token=dummy \
		--set grafana.dashboards.enabled=true --set grafana.dashboards.folder=DigitalOcean >/dev/null
	helm template charts/digitalocean-exporter --set digitalocean.token=dummy \
		--set prometheusRule.enabled=true >/dev/null
# The escape hatches render nothing when unset, which is the whole point of them
# and also why a default render never reaches their branches.
	helm template charts/digitalocean-exporter --set digitalocean.token=dummy \
		--set 'imagePullSecrets[0].name=registry' --set podAnnotations.example=1 \
		--set priorityClassName=low --set 'extraArgs[0]=--do.timeout=20s' >/dev/null
# The spaces collector is the one with credentials of its own, a region and a bucket
# list; `name@region` is the form the region-scoped bucket flag takes.
	helm template charts/digitalocean-exporter --set digitalocean.token=dummy \
		--set collectors.spaces.enabled=true \
		--set spaces.accessKey=key --set spaces.secretKey=secret \
		--set collectors.spaces.region=fra1 \
		--set 'collectors.spaces.buckets[0]=backups' \
		--set 'collectors.spaces.buckets[1]=media@ams3' >/dev/null
# An externally managed Secret with a key name of its own: the branch that renders no
# Secret at all, and mounts a key the chart did not choose.
	helm template charts/digitalocean-exporter \
		--set digitalocean.existingSecret=digitalocean-token \
		--set digitalocean.existingSecretKey=api-token >/dev/null
	helm template charts/digitalocean-exporter --set digitalocean.token=dummy \
		--set serviceMonitor.enabled=true \
		--set serviceMonitor.labels.release=kube-prometheus-stack >/dev/null
# Name collisions, an unstable pod-template checksum and the disabled branch of every
# collector: three things that render without error and are still wrong.
	./scripts/chart-invariants.sh

## chart-docs: Regenerate the chart README from the comments in values.yaml.
chart-docs:
	helm-docs --chart-search-root=charts --template-files=README.md.gotmpl

## docs: Build the documentation site into site/.
docs:
	mkdocs build --strict

## docs-serve: Serve the documentation locally with live reload.
docs-serve:
	mkdocs serve

## clean: Remove build artifacts.
clean:
	rm -rf $(APP_NAME) coverage.out dist/ dist-chart/ site/
