#!/usr/bin/env bash
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

# Applies modular Kubernetes and Agent Substrate cluster-side manifests:
#   hack/deploy-k8s.sh [namespace|computeclass|workerpool|gateway|all]
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

deploy_namespace() {
  step "Applying namespace and RBAC (manifests/namespace.yaml, manifests/rbac.yaml)"
  kubectl apply -f "${REPO_ROOT}/manifests/namespace.yaml"
  kubectl apply -f "${REPO_ROOT}/manifests/rbac.yaml"
  ok "Namespace '${NAMESPACE}' and factorio-gateway RBAC ready"
}

deploy_computeclass() {
  step "Applying ComputeClass (manifests/computeclass.yaml, zones=${WORKER_ZONES})"
  render_computeclass_yaml | kubectl apply -f -
  ok "ComputeClass factorio-substrate-worker ready"
}

ensure_atelet_eviction_and_delays() {
  kubectl get ds -n ate-system atelet >/dev/null 2>&1 \
    || die "could not find atelet DaemonSet in namespace ate-system — run 'make install-substrate' first"
  kubectl patch ds -n ate-system atelet \
    -p '{"spec":{"template":{"metadata":{"annotations":{"cluster-autoscaler.kubernetes.io/safe-to-evict":"true"}}}}}' \
    >/dev/null 2>&1 || true
  if ! kubectl get ds -n ate-system atelet -o jsonpath='{.spec.template.spec.containers[0].args[*]}' 2>/dev/null | grep -q -- '--snapshot-upload-delay='; then
    step "Configuring atelet background snapshot upload (2s) and detach (30s) delays for scale-to-zero"
    kubectl patch ds -n ate-system atelet --type=json \
      -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--snapshot-upload-delay=2s"},{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--snapshot-detach-delay=30s"}]'
    kubectl rollout status ds/atelet -n ate-system --timeout=120s
  fi
}

deploy_workerpool() {
  deploy_namespace
  ensure_atelet_eviction_and_delays
  resolve_worker_image
  step "Deploying WorkerPool/factorio (replicas=${MAX_ACTIVE_WORLDS}, image=${WORKER_IMAGE})"
  render_workerpool_yaml | kubectl apply -f -
  kubectl patch workerpool factorio -n "${NAMESPACE}" --type=merge \
    -p "{\"spec\":{\"replicas\":${MAX_ACTIVE_WORLDS}}}" >/dev/null 2>&1 || true
  kubectl wait --for=create "deployment/factorio" -n "${NAMESPACE}" --timeout=120s
  kubectl rollout status "deployment/factorio" -n "${NAMESPACE}" --timeout=360s
  ok "WorkerPool/factorio Ready"
}

deploy_gateway() {
  deploy_namespace
  ensure_udp_firewall_rule
  if [[ -z "$(kubectl get nodes -l "cloud.google.com/gke-nodepool=${CONTROL_POOL_NAME}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)" ]]; then
    die "no node found matching cloud.google.com/gke-nodepool=${CONTROL_POOL_NAME} — run 'make optimize-cluster' first (or override CONTROL_POOL_NAME)"
  fi
  if kubectl get svc -n "${NAMESPACE}" factorio-gateway >/dev/null 2>&1; then
    step "Deleting legacy factorio-gateway LoadBalancer Service (using hostPort ingress)"
    kubectl delete svc -n "${NAMESPACE}" factorio-gateway --ignore-not-found
  fi
  resolve_worlds
  ensure_ko
  step "Building and deploying factorio-gateway (IDLE_TIMEOUT=${IDLE_TIMEOUT}, WORKERPOOL_IDLE_TIMEOUT=${WORKERPOOL_IDLE_TIMEOUT}, MAX_ACTIVE_WORLDS=${MAX_ACTIVE_WORLDS}, WORLDS=${#WORLDS[@]})"
  render_gateway_yaml | run_ko resolve -f - | kubectl apply -f -
  kubectl rollout status deployment/factorio-gateway -n "${NAMESPACE}" --timeout=300s
  ok "factorio-gateway Ready"
}

TARGET="${1:-all}"
case "${TARGET}" in
  namespace)    deploy_namespace ;;
  computeclass) deploy_computeclass ;;
  workerpool)   deploy_workerpool ;;
  gateway)      deploy_gateway ;;
  all)
    deploy_namespace
    deploy_computeclass
    deploy_workerpool
    deploy_gateway
    ;;
  *)
    die "usage: hack/deploy-k8s.sh [namespace|computeclass|workerpool|gateway|all]"
    ;;
esac
