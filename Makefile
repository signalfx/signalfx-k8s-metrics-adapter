REGISTRY?=quay.io/signalfx
IMAGE?=k8s-metrics-adapter
ARCH?=amd64
OS?=linux

VERSION?=latest

.PHONY: all adapter test image

all: adapter
adapter:
	CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) go build -o ./adapter ./cmd/adapter

test:
	CGO_ENABLED=0 go test -v ./cmd/... ./internal/...

image:
	docker build -t $(REGISTRY)/$(IMAGE):$(VERSION) .
	if [[ "$(PUSH)" == "yes" ]]; then docker push $(REGISTRY)/$(IMAGE):$(VERSION); fi
