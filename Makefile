# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

.PHONY: help cluster gcp install-substrate optimize-cluster \
	observability observability-off observability-on observability-status \
	namespace computeclass workerpool gateway actor-template actors deploy up \
	test build verify verify-with-idle prove test-preemption test-preemption-zone-failover \
	watch watch-transitions status check-update \
	teardown delete-gcp delete-cluster destroy clean

help: ## Show available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "  Configure '.env' first: cp .env.example .env (set PROJECT_ID)"

cluster: ## Create the GKE cluster in .env (CLUSTER_NAME, CLUSTER_LOCATION)
	@./hack/create-cluster.sh

gcp: ## Create GCS bucket, Artifact Registry repo, IAM bindings, & UDP firewall rule
	@./hack/gcp-resources.sh

install-substrate: ## Install Agent Substrate (ate-system) & build bin/kubectl-ate
	@./hack/install-substrate.sh

optimize-cluster: ## Migrate to Spot control-pool, compact Postgres (10Gi), & multi-zone ComputeClass
	@./hack/setup-cost-optimized-pools.sh

observability: ## Apply observability settings from .env (GKE logging/monitoring, Prometheus, ATE log level)
	@./hack/observability.sh apply

observability-off: ## Silence ATE/workload Cloud Logging, set ate-api-server=error, & disable Managed Prometheus
	@./hack/observability.sh off

observability-on: ## Re-enable workload Cloud Logging, ate-api-server=info (or ATE_API_LOG_LEVEL=debug), & Managed Prometheus
	@./hack/observability.sh on

observability-status: ## Show current GKE logging, Managed Prometheus, and ate-api-server log level
	@./hack/observability.sh status

namespace: ## Apply manifests/namespace.yaml and manifests/rbac.yaml
	@./hack/deploy-k8s.sh namespace

computeclass: ## Apply multi-zone Spot ComputeClass (manifests/computeclass.yaml)
	@./hack/deploy-k8s.sh computeclass

workerpool: ## Build/resolve worker image & apply WorkerPool (manifests/workerpool.yaml)
	@./hack/deploy-k8s.sh workerpool

gateway: ## Build & deploy factorio-gateway (manifests/gateway.yaml)
	@./hack/deploy-k8s.sh gateway

actor-template: ## Register factorio-server ActorTemplate & wait for Golden Snapshot
	@./hack/deploy-actors.sh template

actors: ## Reconcile factorio Atespace & world Actors (manifests/actors.yaml)
	@./hack/deploy-actors.sh actors

deploy: ## Deploy full Factorio workload (namespace -> computeclass -> workerpool -> gateway -> template -> actors)
	@./hack/deploy.sh

up: cluster gcp install-substrate optimize-cluster deploy ## Provision cluster, GCP, Substrate, cost-optimized pools, & Factorio end-to-end

test: ## Run Go vet, unit tests (-race), shell checks, and YAML manifest validation
	go vet ./...
	go test -v -race ./...
	./hack/test-scripts.sh

build: ## Build local Go binaries into bin/ (sidecar, gateway, probe)
	mkdir -p bin
	go build -o bin/factorio-sidecar ./cmd/factorio-sidecar
	go build -o bin/factorio-gateway ./cmd/factorio-gateway
	go build -o bin/factorio-probe ./cmd/factorio-probe

verify: ## Run E2E UDP wake, RCON status, & multi-world multiplexing test
	@./hack/test-e2e.sh

verify-with-idle: ## Verify 30s idle auto-suspend & restore 10m IDLE_TIMEOUT
	@IDLE_TIMEOUT=30s ./hack/deploy-k8s.sh gateway
	@VERIFY_IDLE_SUSPEND=true IDLE_WAIT_SECS=40 ./hack/test-e2e.sh
	@IDLE_TIMEOUT=10m ./hack/deploy-k8s.sh gateway

prove: ## Run full stateful RCON chest + GCS eviction/restore proof
	@./hack/prove-e2e.sh

test-preemption: ## Simulate 2-hop Spot preemption cascade [SIMULATE_ZONE_STOCKOUT=true]
	@./hack/test-spot-cascade-preemption.sh

test-preemption-zone-failover: ## Simulate 2-hop Spot preemption with zonal stockout (forces cross-zone failover)
	@SIMULATE_ZONE_STOCKOUT=true ./hack/test-spot-cascade-preemption.sh

watch: ## Watch Actors, WorkerPool, Nodes, & gateway status live
	@./hack/watch-actors.sh

watch-transitions: ## Stream timestamped Actor state transitions
	@./hack/watch-actors.sh --transitions

status: ## Show live factorio-gateway HTTP /status JSON
	@kubectl get --raw /api/v1/namespaces/factorio/services/factorio-gateway-http:status-http/proxy/status | jq .

check-update: ## Trigger immediate upstream Factorio image check on factorio-gateway
	@kubectl get --raw /api/v1/namespaces/factorio/services/factorio-gateway-http:status-http/proxy/check-update | jq .

teardown: ## Delete Factorio actors, templates, namespace, RBAC, ComputeClass [ALL=1 (also bucket/AR/firewall/GSA) YES=1]
	@./hack/teardown.sh $(if $(ALL),--all)

delete-gcp: ## Delete GCS bucket, Artifact Registry repo, UDP firewall rule, & transfer GSA [YES=1]
	@./hack/teardown.sh --gcp-only

delete-cluster: ## Delete GKE cluster & release PVCs/disks [YES=1 DELETE_ORPHAN_DISKS=true]
	@./hack/delete-cluster.sh

destroy: ## Tear down Factorio + GCP resources (ALL=1) and delete the GKE cluster [YES=1]
	@./hack/teardown.sh --all
	@./hack/delete-cluster.sh

clean: ## Remove local Go build artifacts (bin/)
	rm -rf bin/
