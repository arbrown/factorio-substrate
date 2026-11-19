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

# Installs Agent Substrate (`ate-system`) onto the target GKE cluster via
# `cmd/ate-setup`, builds `bin/kubectl-ate`, and resolves the digest-pinned
# gVisor worker image for WorkerPool/factorio.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env
ensure_substrate_checkout
ensure_kubectl_ate

step "Verifying cluster prerequisites"
kubectl get nodes >/dev/null 2>&1 || die "cannot reach GKE cluster; run 'make cluster' or check kubeconfig"
cert_apis="$(kubectl get --raw /apis/certificates.k8s.io/v1 2>/dev/null; kubectl get --raw /apis/certificates.k8s.io/v1beta1 2>/dev/null || true)"
if [[ "${cert_apis}" != *"\"clustertrustbundles\""* || "${cert_apis}" != *"\"podcertificaterequests\""* ]]; then
  die "cluster does not serve ClusterTrustBundle + PodCertificateRequest (use GKE 1.37+ or enable v1beta1 unstable APIs)"
fi
ok "Cluster reachable and podcertificate APIs served"

step "Deploying Agent Substrate (${SUBSTRATE_REF}) control plane into ate-system"
setup_args=(
  --no-dev-env
  --sandbox-classes="${ATE_INSTALL_SANDBOX_CLASSES}"
  --credential-provider="${ATE_CREDENTIAL_PROVIDER}"
  --snapshot-location="${SNAPSHOT_LOCATION}"
  --snapshot-transfer-gsa-name="${SNAPSHOT_TRANSFER_GSA_NAME}"
  --ingress-allowed-spiffe-ids="${ATE_INGRESS_ALLOWED_SPIFFE_IDS}"
  --ateapi-authz-bootstrap-owners="${ATE_API_AUTHZ_BOOTSTRAP_OWNERS}"
  --ateapi-log-level="${ATE_API_LOG_LEVEL}"
  --gcp-project-id="${PROJECT_ID}"
  --gcp-cluster-location="${CLUSTER_LOCATION}"
  --gcp-cluster-name="${CLUSTER_NAME}"
  --rollout-timeout="${ATE_ROLLOUT_TIMEOUT}"
)
if [[ "${ATE_INSTALL_E2E_TUNING}" == "true" ]]; then
  setup_args+=(--e2e-tuning)
fi

if [[ -n "${SUBSTRATE_IMAGE_REPO:-}" ]]; then
  tag="${SUBSTRATE_IMAGE_TAG:-${SUBSTRATE_REF}}"
  step "Installing published images from ${SUBSTRATE_IMAGE_REPO}:${tag}"
  setup_args+=(--image-repo="${SUBSTRATE_IMAGE_REPO}" --image-tag="${tag}")
else
  ensure_ko
  step "Building and pushing Substrate images from source (${KO_DOCKER_REPO})"
  setup_args+=(--ko-docker-repo="${KO_DOCKER_REPO}" --ko-default-platforms="${KO_DEFAULTPLATFORMS}")
fi

(cd "${SUBSTRATE_SRC}" && go run ./cmd/ate-setup deploy ate-system "${setup_args[@]}")

resolve_worker_image
ok "Worker image resolved: ${WORKER_IMAGE}"

kubectl patch ds -n ate-system atelet \
  -p '{"spec":{"template":{"metadata":{"annotations":{"cluster-autoscaler.kubernetes.io/safe-to-evict":"true"}}}}}' \
  >/dev/null 2>&1 || true

step "Waiting for ate-system workloads"
kubectl -n ate-system wait --for=condition=Available deploy --all --timeout=10m
kubectl -n ate-system rollout status ds/atelet --timeout=10m
ok "Agent Substrate installed and ready"
