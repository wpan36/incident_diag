# syntax=docker/dockerfile:1

# One image definition for every binary in this repository. The CMD build
# argument names the package under cmd/, so the two Incident Lab services and
# ops-mcp share this file rather than each having a near-identical copy.
#
#   docker build --build-arg CMD=checkout-service -t checkout-service .
#
# Compose passes the argument; see deploy/docker-compose.yml.

ARG GO_VERSION=1.26.5

FROM golang:${GO_VERSION} AS build
WORKDIR /src

# The module files alone first, so a change to the source does not re-download
# every dependency.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG CMD
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "$CMD" || { echo "the CMD build argument is required"; exit 1; } && \
    CGO_ENABLED=0 go build -trimpath -o /out/service ./cmd/${CMD}

# Static, so there is nothing in the image but the binary and CA certificates.
# There is also no shell, which is why the lab services have no Compose
# healthcheck: the scenario tool waits on /health instead.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
ENTRYPOINT ["/service"]
