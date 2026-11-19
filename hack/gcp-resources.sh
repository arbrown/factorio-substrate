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

# Provisions GCP resources needed by Agent Substrate and Factorio:
# GCS snapshot bucket, Artifact Registry repo (when AR_REPO is set),
# snapshot-transfer GSA, Workload Identity IAM bindings, and UDP firewall rule.
# Safe to re-run.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" --project="${PROJECT_ID}" --format='value(projectNumber)')"
WI="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject"
COMPUTE_SA="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"

step "Enabling required GCP APIs"
gcloud services enable --project "${PROJECT_ID}" \
  compute.googleapis.com \
  container.googleapis.com \
  storage.googleapis.com \
  artifactregistry.googleapis.com \
  iam.googleapis.com \
  iamcredentials.googleapis.com \
  cloudresourcemanager.googleapis.com >/dev/null
ok "APIs enabled"

step "Snapshot bucket gs://${BUCKET_NAME}"
if ! gcloud storage buckets describe "gs://${BUCKET_NAME}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud storage buckets create "gs://${BUCKET_NAME}" \
    --project "${PROJECT_ID}" \
    --location "${REGION}" \
    --uniform-bucket-level-access >/dev/null
  ok "Created gs://${BUCKET_NAME}"
else
  ok "gs://${BUCKET_NAME} already exists"
fi

if [[ -n "${AR_REPO:-}" ]]; then
  step "Artifact Registry ${AR_REPO} (${REGION})"
  if ! gcloud artifacts repositories describe "${AR_REPO}" \
      --location "${REGION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
    gcloud artifacts repositories create "${AR_REPO}" \
      --repository-format=docker \
      --location "${REGION}" \
      --project "${PROJECT_ID}" >/dev/null
    ok "Created Artifact Registry repo ${AR_REPO}"
  else
    ok "Artifact Registry repo ${AR_REPO} already exists"
  fi
  gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet >/dev/null
elif [[ "${KO_DOCKER_REPO}" == gcr.io/* ]]; then
  step "Configuring Docker auth for gcr.io"
  gcloud auth configure-docker gcr.io --quiet >/dev/null
  ok "Docker configured for ${KO_DOCKER_REPO}"
fi

_retry_gcloud() {
  local i
  for ((i = 1; i <= 6; i++)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    (( i < 6 )) && sleep 5
  done
  "$@" >/dev/null
}

bind_bucket() {
  _retry_gcloud gcloud storage buckets add-iam-policy-binding "gs://${BUCKET_NAME}" \
    --project="${PROJECT_ID}" --member="$1" --role="$2" --condition=None
}
bind_gsa() {
  _retry_gcloud gcloud iam service-accounts add-iam-policy-binding "$1" \
    --project="${PROJECT_ID}" --member="$2" --role="$3" --condition=None
}
bind_repo() {
  _retry_gcloud gcloud artifacts repositories add-iam-policy-binding "${AR_REPO}" \
    --location="${REGION}" --project="${PROJECT_ID}" --member="$1" --role="$2"
}

TRANSFER_GSA_EMAIL="${SNAPSHOT_TRANSFER_GSA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
step "Snapshot transfer GSA (${TRANSFER_GSA_EMAIL})"
if ! gcloud iam service-accounts describe "${TRANSFER_GSA_EMAIL}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud iam service-accounts create "${SNAPSHOT_TRANSFER_GSA_NAME}" \
    --project "${PROJECT_ID}" \
    --display-name "Substrate snapshot transfers" >/dev/null
  ok "Created ${TRANSFER_GSA_EMAIL}"
else
  ok "${TRANSFER_GSA_EMAIL} already exists"
fi

step "Binding IAM for Substrate Workload Identity principals and GCE node SA"
gcloud container clusters describe "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1 \
  || die "GKE cluster '${CLUSTER_NAME}' not found in ${CLUSTER_LOCATION} — run 'make cluster' before 'make gcp' so the ${PROJECT_ID}.svc.id.goog Workload Identity pool exists"
for sa in atelet ate-api-server; do
  bind_bucket "${WI}/ns/ate-system/sa/${sa}" roles/storage.objectAdmin
  bind_bucket "${WI}/ns/ate-system/sa/${sa}" roles/storage.bucketViewer
  if [[ -n "${AR_REPO:-}" ]]; then
    bind_repo "${WI}/ns/ate-system/sa/${sa}" roles/artifactregistry.reader
  fi
done
bind_bucket "serviceAccount:${TRANSFER_GSA_EMAIL}" roles/storage.objectAdmin
bind_bucket "serviceAccount:${TRANSFER_GSA_EMAIL}" roles/storage.bucketViewer
bind_gsa "${TRANSFER_GSA_EMAIL}" "${WI}/ns/ate-system/sa/ate-api-server" roles/iam.serviceAccountTokenCreator
bind_bucket "${COMPUTE_SA}" roles/storage.objectViewer
if [[ -n "${AR_REPO:-}" ]]; then
  bind_repo "${COMPUTE_SA}" roles/artifactregistry.reader
fi
ok "IAM bindings applied"

if gcloud compute networks describe default --project="${PROJECT_ID}" >/dev/null 2>&1; then
  ensure_udp_firewall_rule
else
  warn "VPC network 'default' not found yet; firewall rule '${FIREWALL_RULE_NAME}' will be created during gateway deploy"
fi
