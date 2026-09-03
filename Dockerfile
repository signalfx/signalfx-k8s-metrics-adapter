ARG GO_VERSION=1.26.8
FROM golang:${GO_VERSION} AS builder

# upgrade to get latest root CA
RUN apt-get update && \
    apt upgrade -y

WORKDIR /opt/app
COPY go.* ./
RUN go mod download

COPY Makefile ./
COPY ./cmd/ ./cmd/
COPY ./internal ./internal/

ARG ARCH=amd64
ENV ARCH=${ARCH}

RUN make adapter

# FINAL IMAGE
FROM busybox:1.31

ENTRYPOINT ["/adapter"]
COPY --from=builder /etc/ssl/certs /etc/ssl/certs
COPY --from=builder /opt/app/adapter /adapter
