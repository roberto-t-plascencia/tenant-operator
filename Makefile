IMG ?= ghcr.io/roberto-t-plascencia/tenant-operator:latest
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2
KIND_CLUSTER ?= tenant-operator

.PHONY: all
all: generate manifests fmt vet test build

##@ Development

.PHONY: generate
generate: ## Generate DeepCopy methods for the API types.
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: manifests
manifests: ## Generate the CRD and ClusterRole from code markers.
	$(CONTROLLER_GEN) crd rbac:roleName=manager-role paths="./..." \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: ## Run unit tests with the race detector.
	go test -race -coverprofile cover.out ./...

##@ Build

.PHONY: build
build:
	go build -o bin/manager ./cmd

.PHONY: run
run: ## Run the controller against the current kubeconfig context.
	go run ./cmd

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

##@ Deploy

.PHONY: install
install: ## Install the CRD into the current cluster.
	kubectl apply -k config/crd

.PHONY: deploy
deploy: ## Deploy the operator into the current cluster.
	kubectl apply -k config/default

.PHONY: undeploy
undeploy:
	kubectl delete -k config/default --ignore-not-found

.PHONY: e2e
e2e: ## Create a kind cluster and run the end-to-end test against it.
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER)
	KIND_CLUSTER=$(KIND_CLUSTER) ./test/e2e/run.sh

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
