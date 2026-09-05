# syntax=docker/dockerfile:1

# ---- builder ----------------------------------------------------------------
# Pinned to match the `go` directive in go.mod exactly.
FROM golang:1.25.11 AS builder

WORKDIR /src

# Dependencies first for layer caching.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Source.
COPY . .

# Fully static, stripped, reproducible binary (no cgo => runs on scratch).
ARG VERSION=docker
RUN CGO_ENABLED=0 GOFLAGS="-trimpath" \
    go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/aimon ./cmd/aimon

# Create a passwd entry for a non-root user (scratch has no user database) and
# a data dir the runtime can own.
RUN printf 'nonroot:x:65532:65532:nonroot:/home/nonroot:/sbin/nologin\n' > /out/passwd \
    && mkdir -p /out/data

# ---- final (scratch) --------------------------------------------------------
FROM scratch

LABEL org.opencontainers.image.source="https://github.com/algonode/aimon" \
      org.opencontainers.image.description="aimon — AI subscription & credit monitor" \
      org.opencontainers.image.licenses="MIT"

# TLS roots (provider APIs are all HTTPS) + timezone data + the user database.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /out/passwd /etc/passwd

# Writable data dir owned by the non-root user (sqlite lives here; bind-mounted).
COPY --from=builder --chown=65532:65532 /out/data /data
COPY --from=builder --chown=65532:65532 /out/aimon /usr/local/bin/aimon

USER 65532:65532
WORKDIR /data
EXPOSE 8080 1455

ENTRYPOINT ["/usr/local/bin/aimon", "--config", "/etc/aimon/config.yaml"]
