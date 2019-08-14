REGISTRY?=quay.io/signalfx
IMAGE?=k8s-metrics-adapter

VERSION?=latest

.PHONY: all adapter test image

all: adapter
adapter:
	CGO_ENABLED=0 GOARCH=$(ARCH) go build -o ./adapter ./cmd/adapter

test:
	CGO_ENABLED=0 go test ./cmd/... ./internal/...

image:
	docker build -t $(REGISTRY)/$(IMAGE):$(VERSION) .
	if [[ "$(PUSH)" == "yes" ]]; then docker push $(REGISTRY)/$(IMAGE):$(VERSION); fi
