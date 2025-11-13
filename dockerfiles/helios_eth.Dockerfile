FROM rust:latest AS builder
WORKDIR /app
RUN git clone https://github.com/a16z/helios.git
WORKDIR /app/helios
# RUN cargo build --release
RUN cargo build

FROM debian:sid-slim AS runtime
# COPY --from=builder /app/helios/target/release/helios /usr/local/bin/
COPY --from=builder /app/helios/target/debug/helios /usr/local/bin/
# ENTRYPOINT ["helios", "ethereum", "--execution-rpc", "https://ethereum-rpc.publicnode.com"]
