FROM debian:bookworm
run apt-get upgrade && apt-get update -y
RUN apt-get install build-essential git-lfs chrony -y
RUN git clone https://github.com/status-im/nimbus-eth2
WORKDIR /nimbus-eth2
RUN openssl rand -hex 32 > jwt.hex
RUN make -j4 nimbus_light_client
RUN ln ./build/nimbus_light_client nimbus
COPY ./config/nimbus/jwt.hex .
# CMD ["./build/nimbus_light_client"]
