SHELL=/bin/bash
.SHELLFLAGS = -o pipefail -c

REGISTRY?=quay.io/signalfx
IMAGE?=k8s-metrics-adapter
ARCH?=amd64
OS?=linux
GO_VERSION?=$(shell awk '/^go / { print $$2; exit }' go.mod)
VERSION?=latest

# Versions used by CI setup actions and the optional local installer.
KIND_VERSION?=v0.33.0
HELM_VERSION?=v4.3.0
LOCALBIN?=$(CURDIR)/bin
KIND?=kind
HELM?=helm
KUBECTL?=kubectl
K8S_VERSION?=$(shell jq -r '.kubernetes_test["k8s-kind-version"][0]' ci-matrix.json)
KIND_NODE_IMAGE?=kindest/node:$(K8S_VERSION)
KIND_CLUSTER_NAME?=metrics-adapter-test-$(subst .,-,$(K8S_VERSION))
KUBECONFIG_TEST?=$(CURDIR)/.test-cluster/kubeconfig
TEST_ENV=KUBECONFIG="$(KUBECONFIG_TEST)"

.PHONY: all adapter test image install-tools ci-tool-versions kind-setup kind-delete test-integration test-helm

all: adapter
adapter:
	CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) go build -o ./adapter ./cmd/adapter

test:
	CGO_ENABLED=0 go test -v -count=1 -timeout=10m ./cmd/... ./internal/...

image:
	docker build --platform=$(OS)/$(ARCH) --build-arg GO_VERSION=$(GO_VERSION) --build-arg ARCH=$(ARCH) -t $(REGISTRY)/$(IMAGE):$(VERSION) .
	if [ "$(PUSH)" = "yes" ]; then docker push $(REGISTRY)/$(IMAGE):$(VERSION); fi

install-tools: ## Optionally install pinned kubectl, Kind, and Helm into LOCALBIN.
	LOCALBIN="$(LOCALBIN)" K8S_VERSION=$(K8S_VERSION) KIND_VERSION=$(KIND_VERSION) HELM_VERSION=$(HELM_VERSION) bash ci_scripts/install-tools.sh

ci-tool-versions:
	@printf 'kind-version=%s\nhelm-version=%s\n' "$(KIND_VERSION)" "$(HELM_VERSION)"

kind-setup: ## Create an isolated Kind cluster using existing tools on PATH.
	@if $(KIND) get clusters | grep -qx "$(KIND_CLUSTER_NAME)"; then echo "Cluster already exists; run make kind-delete with the same K8S_VERSION first." >&2; exit 1; fi
	mkdir -p "$(dir $(KUBECONFIG_TEST))"
	$(KIND) create cluster --name=$(KIND_CLUSTER_NAME) --image=$(KIND_NODE_IMAGE) --kubeconfig="$(KUBECONFIG_TEST)" --wait=180s
	$(TEST_ENV) $(KUBECTL) wait --for=condition=Ready nodes --all --timeout=180s

kind-delete: ## Delete the selected test cluster.
	$(TEST_ENV) $(KIND) delete cluster --name=$(KIND_CLUSTER_NAME)

test-integration: kind-setup ## Create a Kind cluster and run the Go integration tests.
	$(TEST_ENV) $(MAKE) test

test-helm: ## Load the built adapter image and test Helm installation on the selected cluster.
	$(KIND) load docker-image $(REGISTRY)/$(IMAGE):$(VERSION) --name=$(KIND_CLUSTER_NAME)
	$(TEST_ENV) $(HELM) upgrade --install adapter-test ./deployments/helm/signalfx-k8s-metrics-adapter --set accessToken=testing123 --set image=$(REGISTRY)/$(IMAGE) --set imageTag=$(VERSION) --set imagePullPolicy=Never --wait --timeout=3m
	$(TEST_ENV) $(KUBECTL) wait --for=condition=Available apiservice/v1beta1.custom.metrics.k8s.io apiservice/v1beta2.custom.metrics.k8s.io apiservice/v1beta1.external.metrics.k8s.io --timeout=180s
	$(TEST_ENV) $(HELM) get manifest adapter-test | $(TEST_ENV) $(KUBECTL) get -f -
