# vtessera, built and shipped as a single self-contained image.
#
# The service is a long-running HTTP server with two pieces of state that must
# outlive a container: the SQLite database, and the Ed25519 marketplace signing
# key. Both live in /data. Run this with that path on a real volume, or every
# restart silently becomes a new marketplace with a new identity.

# ---- build ----------------------------------------------------------------

FROM docker.io/library/golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module cache.
# There are no third-party services in the build; this is a pure Go compile.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 is what keeps the runtime image tiny: the SQLite driver
# (modernc.org/sqlite) is pure Go, so nothing needs a C library at runtime and
# the result is a static binary.
#
# -trimpath strips the build machine's paths from the binary, so a stack trace
# from production names the module, not the maintainer's home directory.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o /out/vtessera ./cmd/vtessera

# ---- runtime --------------------------------------------------------------

FROM docker.io/library/alpine:3.21

# ca-certificates: the service makes outbound TLS calls to the Solana RPC
# endpoint. tzdata: timestamps in logs. Both are small; a debugging session
# without them is not.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /data vtessera \
 && mkdir -p /data \
 && chown vtessera:vtessera /data

COPY --from=build /out/vtessera /usr/local/bin/vtessera

# Never run a network service as root. The process only needs to write /data.
USER vtessera
WORKDIR /data

# Declaring it documents the contract; the orchestrator still has to mount it.
VOLUME /data

EXPOSE 8080

ENV VTESSERA_ADDR=:8080 \
    VTESSERA_DB=/data/vtessera.db \
    VTESSERA_SIGNER_KEY=/data/signer.key

# busybox wget is already in the image, so this needs nothing installed.
# /healthz is cheap and does not touch the database.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/vtessera"]
