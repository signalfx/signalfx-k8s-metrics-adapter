FROM golang:1.12 as builder

WORKDIR /opt/app
COPY go.* ./
RUN go mod download

COPY Makefile ./
COPY ./cmd/ ./cmd/
COPY ./internal ./internal/

RUN make adapter


# FINAL IMAGE
FROM busybox:1.31

ENTRYPOINT ["/adapter"]
COPY --from=builder /etc/ssl/certs /etc/ssl/certs
COPY --from=builder /opt/app/adapter /adapter
