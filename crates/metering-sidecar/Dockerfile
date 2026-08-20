FROM rust:1.75-slim as builder

WORKDIR /app

COPY Cargo.toml Cargo.lock ./
COPY crates/executor/Cargo.toml crates/executor/Cargo.toml
COPY crates/metering-sidecar/Cargo.toml crates/metering-sidecar/Cargo.toml

RUN mkdir -p crates/executor/src && echo "pub fn dummy() {}" > crates/executor/src/lib.rs
RUN mkdir -p crates/metering-sidecar/src && echo "fn main() {}" > crates/metering-sidecar/src/main.rs

RUN cargo build --release --bin metering-sidecar 2>/dev/null || true

COPY crates/executor/src crates/executor/src
COPY crates/metering-sidecar/src crates/metering-sidecar/src

RUN touch crates/executor/src/lib.rs crates/metering-sidecar/src/main.rs && \
    cargo build --release --bin metering-sidecar

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    nvidia-smi 2>/dev/null || true

COPY --from=builder /app/target/release/metering-sidecar /usr/local/bin/metering-sidecar

ENTRYPOINT ["/usr/local/bin/metering-sidecar"]
