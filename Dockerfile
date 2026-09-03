# Cross-compile rather than emulate: the builder always runs natively on the
# build platform and only GOARCH changes, so the arm64 image costs the same as
# amd64. Building arm64 under QEMU would be several times slower.
#
# Both base images are pinned by digest: a tag moves with every upstream rebuild,
# and a digest is the only reference that names one exact image. The tag each
# digest was taken from is the comment beside it — update the digest by resolving
# that tag again (`docker buildx imagetools inspect <tag>`).

# golang:1.26-alpine
FROM --platform=$BUILDPLATFORM golang@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS build

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
        -X github.com/kozaktomas/digitalocean_exporter/internal/version.Version=${VERSION} \
        -X github.com/kozaktomas/digitalocean_exporter/internal/version.Commit=${COMMIT_SHA}" \
      -o /out/digitalocean_exporter ./cmd/digitalocean_exporter

# distroless/static ships CA certificates (needed for api.digitalocean.com)
# and no shell.
# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Redeclared: an ARG from before the first FROM or another stage is out of scope here.
ARG VERSION=dev
ARG COMMIT_SHA=none
LABEL org.opencontainers.image.source="https://github.com/kozaktomas/digitalocean_exporter" \
      org.opencontainers.image.description="Prometheus exporter for DigitalOcean" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT_SHA}"

COPY --from=build /out/digitalocean_exporter /usr/bin/digitalocean_exporter

USER nonroot:nonroot
EXPOSE 9212
ENTRYPOINT ["/usr/bin/digitalocean_exporter"]
