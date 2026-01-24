# Kausality - Causal traceability for Kubernetes resource mutations

# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/kausality-io/kausality:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: test-verbose
test-verbose: fmt vet ## Run tests with verbose output.
	go test ./... -v

.PHONY: envtest
envtest: setup-envtest ## Run envtest integration tests.
	KUBEBUILDER_ASSETS="$(shell $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./... -tags=envtest -v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: imports
imports: goimports ## Fix imports in Go files.
	$(GOIMPORTS) -w .

##@ Build

.PHONY: build
build: fmt vet ## Build webhook binary.
	go build -o bin/kausality-webhook ./cmd/kausality-webhook

.PHONY: build-cli
build-cli: fmt vet ## Build CLI binary.
	go build -o bin/kausality-cli ./cmd/kausality-cli

.PHONY: build-backend-tui
build-backend-tui: fmt vet ## Build backend TUI binary.
	go build -o bin/kausality-backend-tui ./cmd/kausality-backend-tui

.PHONY: build-backend-log
build-backend-log: fmt vet ## Build backend logger binary.
	go build -o bin/kausality-backend-log ./cmd/kausality-backend-log

.PHONY: run
run: fmt vet ## Run the webhook from your host (for development).
	go run ./cmd/kausality-webhook

.PHONY: docker-build
docker-build: ## Build docker image.
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image.
	docker push ${IMG}

.PHONY: ko-build
ko-build: ko ## Build images with ko (local).
	KO_DOCKER_REPO=ko.local $(KO) build --bare ./cmd/kausality-webhook
	KO_DOCKER_REPO=ko.local $(KO) build --bare ./cmd/kausality-backend-log

.PHONY: ko-build-kind
ko-build-kind: ko ## Build and load images into kind cluster.
	KIND_CLUSTER_NAME=$${KIND_CLUSTER_NAME:-kausality-e2e} $(KO) build --bare ./cmd/kausality-webhook | xargs -I{} kind load docker-image {} --name $${KIND_CLUSTER_NAME}
	KIND_CLUSTER_NAME=$${KIND_CLUSTER_NAME:-kausality-e2e} $(KO) build --bare ./cmd/kausality-backend-log | xargs -I{} kind load docker-image {} --name $${KIND_CLUSTER_NAME}

##@ E2E Testing

.PHONY: e2e
e2e: ## Run e2e tests with kind.
	./test/e2e/run.sh

.PHONY: e2e-crossplane
e2e-crossplane: ## Run e2e tests with Crossplane.
	./test/e2e/crossplane/run.sh

##@ Deployment

.PHONY: install
install: helm ## Install the webhook to the K8s cluster specified in ~/.kube/config.
	$(HELM) upgrade --install kausality ./charts/kausality

.PHONY: uninstall
uninstall: helm ## Uninstall the webhook from the K8s cluster specified in ~/.kube/config.
	$(HELM) uninstall kausality

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
GOIMPORTS ?= $(LOCALBIN)/goimports
HELM ?= $(LOCALBIN)/helm
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
KO ?= $(LOCALBIN)/ko
KIND ?= $(LOCALBIN)/kind

## Tool Versions
GOLANGCI_LINT_VERSION ?= v2.8.0
HELM_VERSION ?= v3.16.3
ENVTEST_K8S_VERSION ?= 1.31.0
KO_VERSION ?= v0.17.1
KIND_VERSION ?= v0.25.0

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	@test -s $(GOLANGCI_LINT) && $(GOLANGCI_LINT) version --format short | grep -q $(GOLANGCI_LINT_VERSION:v%=%) || \
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION)

.PHONY: goimports
goimports: $(GOIMPORTS) ## Download goimports locally if necessary.
$(GOIMPORTS): $(LOCALBIN)
	@test -s $(GOIMPORTS) || GOBIN=$(LOCALBIN) go install golang.org/x/tools/cmd/goimports@latest

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	@test -s $(HELM) || { \
		curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 && \
		chmod 700 get_helm.sh && \
		HELM_INSTALL_DIR=$(LOCALBIN) USE_SUDO=false ./get_helm.sh --version $(HELM_VERSION) && \
		rm get_helm.sh; \
	}

.PHONY: setup-envtest
setup-envtest: $(SETUP_ENVTEST) ## Download setup-envtest locally if necessary.
$(SETUP_ENVTEST): $(LOCALBIN)
	@test -s $(SETUP_ENVTEST) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: ko
ko: $(KO) ## Download ko locally if necessary.
$(KO): $(LOCALBIN)
	@test -s $(KO) || GOBIN=$(LOCALBIN) go install github.com/google/ko@$(KO_VERSION)

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	@test -s $(KIND) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kind@$(KIND_VERSION)

.PHONY: clean
clean: ## Clean up build artifacts.
	rm -rf bin/ cover.out
