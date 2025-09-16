FROM golang:1.25.1-alpine3.22

RUN apk add --no-cache git

WORKDIR /btcd

RUN git clone https://github.com/btcsuite/btcd.git .
RUN go install -v . ./cmd/...

RUN ls -al

CMD ["btcd"]
