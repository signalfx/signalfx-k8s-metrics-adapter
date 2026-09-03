REGISTRY?=quay.io/signalfx
IMAGE?=k8s-metrics-adapter
ARCH?=amd64
OS?=linux
GO_VERSION?=$(shell awk '/^go / { print $$2; exit }' go.mod)

VERSION?=latest

.PHONY: all adapter test image

all: adapter
adapter:
	CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) go build -o ./adapter ./cmd/adapter

test:
	CGO_ENABLED=0 go test -v ./cmd/... ./internal/...

image:
	docker build --platform=$(OS)/$(ARCH) --build-arg GO_VERSION=$(GO_VERSION) --build-arg ARCH=$(ARCH) -t $(REGISTRY)/$(IMAGE):$(VERSION) .
	if [[ "$(PUSH)" == "yes" ]]; then docker push $(REGISTRY)/$(IMAGE):$(VERSION); fi
