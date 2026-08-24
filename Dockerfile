# Cross-compile rather than emulate: the builder always runs natively on the
# build platform and only GOARCH changes, so the arm64 image costs the same as
# amd64. Building arm64 under QEMU would be several times slower.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT_SHA=none

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -ldflags "-s -w \
        -X github.com/panbotka/digitalocean_exporter/internal/version.Version=${VERSION} \
        -X github.com/panbotka/digitalocean_exporter/internal/version.Commit=${COMMIT_SHA}" \
      -o /out/digitalocean_exporter ./cmd/digitalocean_exporter

# distroless/static ships CA certificates (needed for api.digitalocean.com)
# and no shell.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/digitalocean_exporter /usr/bin/digitalocean_exporter

USER nonroot:nonroot
EXPOSE 9212
ENTRYPOINT ["/usr/bin/digitalocean_exporter"]
