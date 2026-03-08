# Image URL to use all building/pushing image targets
IMG ?= controller:latest
CONN_KILLER_IMG ?= connection-killer:latest
CONNECTION_KILLER_ENABLED ?= true
DEPLOY_NAMESPACE ?= k8s-failover-manager-system

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker
GO_TOOLCHAIN ?= go1.26.1
CUSTOM_GOLANGCI_LINT ?= false

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	@cp config/crd/bases/*.yaml $(CHART_DIR)/crds/

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" GOTOOLCHAIN=$(GO_TOOLCHAIN) go test $$(GOTOOLCHAIN=$(GO_TOOLCHAIN) go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
KIND_CLUSTER ?= k8s-failover-manager-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) GOTOOLCHAIN=$(GO_TOOLCHAIN) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go build -o bin/manager cmd/main.go

.PHONY: build-connection-killer
build-connection-killer: fmt vet ## Build connection-killer binary (Linux only).
	GOOS=linux GOTOOLCHAIN=$(GO_TOOLCHAIN) go build -o bin/connection-killer cmd/connection-killer/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-build-connection-killer
docker-build-connection-killer: ## Build docker image for the connection-killer.
	$(CONTAINER_TOOL) build -t ${CONN_KILLER_IMG} -f Dockerfile.connection-killer .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-push-connection-killer
docker-push-connection-killer: ## Push docker image for the connection-killer.
	$(CONTAINER_TOOL) push ${CONN_KILLER_IMG}

PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name k8s-failover-manager-builder
	$(CONTAINER_TOOL) buildx use k8s-failover-manager-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm k8s-failover-manager-builder
	rm Dockerfile.cross

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	"$(KUBECTL)" apply -f config/crd/bases/

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f config/crd/bases/

.PHONY: deploy
deploy: manifests ## Deploy controller via Helm (set IMG/CONN_KILLER_IMG/CONNECTION_KILLER_ENABLED as needed).
	@set -e; \
	split_image_ref() { \
		local ref="$$1"; \
		if echo "$$ref" | grep -q '@'; then \
			echo "$${ref%%@*} latest $${ref#*@}"; \
		elif echo "$$ref" | grep -qE ':[^/]+$$'; then \
			echo "$${ref%:*} $${ref##*:}"; \
		else \
			echo "$$ref latest"; \
		fi; \
	}; \
	read IMG_REPO IMG_TAG IMG_DIGEST <<< $$(split_image_ref "$${IMG}"); \
	read CK_REPO CK_TAG CK_DIGEST <<< $$(split_image_ref "$${CONN_KILLER_IMG}"); \
	EXTRA_ARGS=""; \
	if [ -n "$${IMG_DIGEST}" ]; then EXTRA_ARGS="$${EXTRA_ARGS} --set image.digest=$${IMG_DIGEST}"; fi; \
	if [ -n "$${CK_DIGEST}" ]; then EXTRA_ARGS="$${EXTRA_ARGS} --set connectionKiller.image.digest=$${CK_DIGEST}"; fi; \
	$(HELM) upgrade --install k8s-failover-manager $(CHART_DIR) \
		--namespace $(DEPLOY_NAMESPACE) --create-namespace \
		--set image.repository="$${IMG_REPO}" \
		--set image.tag="$${IMG_TAG}" \
		--set connectionKiller.enabled="$(CONNECTION_KILLER_ENABLED)" \
		--set connectionKiller.image.repository="$${CK_REPO}" \
		--set connectionKiller.image.tag="$${CK_TAG}" \
		$${EXTRA_ARGS} $(HELM_EXTRA_ARGS)

.PHONY: undeploy
undeploy: ## Undeploy controller Helm release.
	-$(HELM) uninstall k8s-failover-manager -n $(DEPLOY_NAMESPACE)

##@ Helm

HELM ?= helm
CHART_DIR = charts/k8s-failover-manager

.PHONY: helm-sync-crds
helm-sync-crds: manifests ## Copy generated CRDs into the Helm chart crds/ directory (included in `make manifests`).

.PHONY: helm-template
helm-template: ## Render Helm chart templates for visual inspection.
	$(HELM) template k8s-failover-manager $(CHART_DIR) --namespace k8s-failover-manager-system

.PHONY: helm-package
helm-package: ## Package the Helm chart into a .tgz archive.
	$(HELM) package $(CHART_DIR)

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	$(HELM) lint $(CHART_DIR)

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.20.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.11.2

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@if [ "$(CUSTOM_GOLANGCI_LINT)" = "true" ] && [ -f .custom-gcl.yml ]; then \
			echo "Building custom golangci-lint with plugins..." && \
			$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
			mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	fi

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" GOTOOLCHAIN=$(GO_TOOLCHAIN) go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell GOTOOLCHAIN=$(GO_TOOLCHAIN) go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
