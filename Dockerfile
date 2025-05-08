# Build target
# Golang image build can't really use alpine version, ref:
# https://github.com/docker-library/golang/issues/209
FROM golang:1.23.4 AS builder

# Binary compressor, could be a difference of ~100mb vs ~600mb
RUN apt-get update && apt-get install -y wget xz-utils
RUN wget https://github.com/upx/upx/releases/download/v4.0.1/upx-4.0.1-amd64_linux.tar.xz -O upx-4.0.1.tar.xz
RUN tar -xf upx-4.0.1.tar.xz
RUN cp upx-4.0.1-amd64_linux/upx /usr/bin/upx

# Running build as non-root user -- security improvement
RUN useradd -u 10001 app
USER app:app

WORKDIR /app
COPY --chown=app:app cmd/ cmd/
COPY --chown=app:app go.mod go.mod
COPY --chown=app:app go.sum go.sum
COPY --chown=app:app cache.go cache.go
COPY --chown=app:app proxy.go proxy.go

# Creating new direcory so we don't create conflicting output binary files
RUN mkdir bin

# We'll need to set the cache directory to currently-owned directory, since
# otherwise Golang will default to OS-defined path, which we won't have the permission
# at this point
#
# We'll disable the CGO since we'll require completely-static build of Go's image
# since we're deploying to scratch image. We'll also specifically only target linux/x64
# architecture
ENV GOCACHE=/app CGO_ENABLED=0 GOOS=linux GOARCH=amd64

# Build & compress
RUN go build -o ./bin/pgproxy ./cmd/pgproxy
RUN upx --best --lzma ./bin/pgproxy

# Runtime target
FROM scratch

# Target directory is not `/etc` or `/usr` since we will use non-root user
COPY --from=builder /app/bin/pgproxy /go/bin/pgproxy

# TLS certs, so that binaries can do HTTP requests to external services
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Lower user permission to non-root user
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
USER app:app
