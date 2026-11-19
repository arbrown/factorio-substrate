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

# Deletes Factorio on Agent Substrate workload resources (Actors, ActorTemplates,
# Atespace, WorkerPool, Namespace, RBAC, ComputeClass, and snapshots).
# Flags:
#   --all       Also deletes GCS bucket, Artifact Registry repo, UDP firewall rule, and transfer GSA.
#   --gcp-only  Only deletes the GCP resources created by `make gcp` (leaves cluster untouched).
# Set YES=1 to skip the confirmation prompt.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

ALL=false
GCP_ONLY=false
case "${1:-}" in
  "")         ;;
  --all)      ALL=true ;;
  --gcp-only) ALL=true; GCP_ONLY=true ;;
  *)          die "usage: hack/teardown.sh [--all|--gcp-only]" ;;
esac

if [[ "${YES:-}" != "1" && "${YES:-}" != "true" ]]; then
  if [[ "${GCP_ONLY}" == "true" ]]; then
    echo "This deletes GCP resources for ${PROJECT_ID}: gs://${BUCKET_NAME}, AR repo '${AR_REPO:-<none>}',"
    echo "firewall rule '${FIREWALL_RULE_NAME}', and GSA '${SNAPSHOT_TRANSFER_GSA_NAME}'."
  else
    echo "This deletes atespace '${ATESPACE}', namespace '${NAMESPACE}', ComputeClass 'factorio-substrate-worker',"
    echo "and gs://${BUCKET_NAME}/${ATESPACE}/."
    [[ "${ALL}" == "true" ]] && echo "ALSO deletes gs://${BUCKET_NAME}, AR repo '${AR_REPO:-<none>}', firewall '${FIREWALL_RULE_NAME}', and GSA '${SNAPSHOT_TRANSFER_GSA_NAME}'."
  fi
  read -r -p "Type 'yes' to continue: " answer || answer=""
  [[ "${answer}" == "yes" ]] || die "aborted"
fi

if [[ "${GCP_ONLY}" != "true" ]]; then
  if kubectl get ns "${NAMESPACE}" >/dev/null 2>&1 || kubectl get crd workerpools.ate.dev >/dev/null 2>&1; then
    ensure_kubectl_ate || true
    if [[ -x "${KUBECTL_ATE_BIN:-}" ]] && run_kubectl_ate get atespace "${ATESPACE}" >/dev/null 2>&1; then
      step "Deleting Actors, ActorTemplates, and Atespace '${ATESPACE}'"
      existing="$(run_kubectl_ate get actors -a "${ATESPACE}" -o json 2>/dev/null | jq -r '(.actors // .items // [])[].metadata.name' || true)"
      for a in ${existing}; do
        run_kubectl_ate suspend actor "${a}" -a "${ATESPACE}" >/dev/null 2>&1 || true
        run_kubectl_ate delete actor "${a}" -a "${ATESPACE}" >/dev/null 2>&1 || warn "could not delete actor ${a}"
      done
      no_actors() {
        [[ -z "$(run_kubectl_ate get actors -a "${ATESPACE}" -o json 2>/dev/null | jq -r '(.actors // .items // [])[].metadata.name' || true)" ]]
      }
      ( wait_for 60 "actors in ${ATESPACE} to finish deleting" no_actors ) || warn "some actors may still be deleting"

      templates="$(run_kubectl_ate get actor-templates -a "${ATESPACE}" -o json 2>/dev/null | jq -r '(.actorTemplates // [])[].metadata.name' || true)"
      for t in ${templates}; do
        run_kubectl_ate delete actor-template "${t}" -a "${ATESPACE}" >/dev/null 2>&1 || true
      done
      run_kubectl_ate delete atespace "${ATESPACE}" >/dev/null 2>&1 || warn "could not delete atespace ${ATESPACE}"
    else
      warn "Atespace '${ATESPACE}' not found or unreachable — skipping Actor cleanup"
    fi

    step "Deleting Kubernetes resources in '${NAMESPACE}' and ComputeClass"
    kubectl -n "${NAMESPACE}" delete workerpool factorio --ignore-not-found --wait=true >/dev/null 2>&1 || true
    kubectl delete ns "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
    kubectl delete clusterrole factorio-gateway-node-watcher --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete clusterrolebinding factorio-gateway-node-watcher --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete computeclass factorio-substrate-worker --ignore-not-found >/dev/null 2>&1 || true
    ok "Kubernetes resources removed"
  else
    warn "Cluster unreachable or '${NAMESPACE}' already removed — skipping Kubernetes teardown"
  fi

  step "Removing Factorio snapshots under gs://${BUCKET_NAME}/${ATESPACE}/"
  gcloud storage rm -r "gs://${BUCKET_NAME}/${ATESPACE}/**" --project="${PROJECT_ID}" --quiet >/dev/null 2>&1 || true
fi

if [[ "${ALL}" == "true" ]]; then
  step "Deleting GCP firewall rule, bucket, Artifact Registry repo, and snapshot GSA in ${PROJECT_ID}"
  gcloud compute firewall-rules delete "${FIREWALL_RULE_NAME}" --project="${PROJECT_ID}" --quiet >/dev/null 2>&1 || true
  gcloud storage rm -r "gs://${BUCKET_NAME}" --project="${PROJECT_ID}" --quiet >/dev/null 2>&1 || true
  gcloud storage rm -r "gs://${BUCKET_NAME}-assets" --project="${PROJECT_ID}" --quiet >/dev/null 2>&1 || true
  if [[ -n "${AR_REPO:-}" ]]; then
    gcloud artifacts repositories delete "${AR_REPO}" --location="${REGION}" --project="${PROJECT_ID}" --quiet >/dev/null 2>&1 || true
  fi
  transfer_gsa="${SNAPSHOT_TRANSFER_GSA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
  gcloud iam service-accounts delete "${transfer_gsa}" --project="${PROJECT_ID}" --quiet >/dev/null 2>&1 || true
  rm -f "${STATE_DIR}/worker-image.${PROJECT_ID}"
  ok "GCP resources removed"
fi
