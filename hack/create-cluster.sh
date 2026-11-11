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

# Creates the initial GKE cluster with Workload Identity, Spot bootstrap pool,
# and PodCertificateRequest support (native on GKE 1.37+, beta API flag on 1.36).
# Idempotent: refreshes credentials if the cluster already exists.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

step "Enabling core GCP APIs in ${PROJECT_ID}"
gcloud services enable --project "${PROJECT_ID}" \
  compute.googleapis.com \
  container.googleapis.com \
  artifactregistry.googleapis.com \
  iam.googleapis.com \
  iamcredentials.googleapis.com \
  cloudresourcemanager.googleapis.com \
  storage.googleapis.com >/dev/null
ok "APIs enabled"

if gcloud container clusters describe "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  ok "Cluster ${CLUSTER_NAME} (${CLUSTER_LOCATION}) already exists"
else
  step "Creating GKE cluster ${CLUSTER_NAME} in ${CLUSTER_LOCATION} (version=${GKE_CLUSTER_VERSION}, pool=${CLUSTER_INIT_MACHINE_TYPE} Spot)"
  create_args=(
    --project="${PROJECT_ID}"
    --zone="${CLUSTER_LOCATION}"
    --release-channel="${GKE_CHANNEL}"
    --cluster-version="${GKE_CLUSTER_VERSION}"
    --machine-type="${CLUSTER_INIT_MACHINE_TYPE}"
    --spot
    --num-nodes=1
    --disk-type=pd-standard
    --disk-size=50
    --workload-pool="${PROJECT_ID}.svc.id.goog"
    --autoscaling-profile=optimize-utilization
    --tags="${CLUSTER_NODE_TAGS}"
  )

  # PodCertificateRequest/ClusterTrustBundle moved to certificates.k8s.io/v1 in GKE 1.37.
  ver_mm="${GKE_CLUSTER_VERSION%%-*}"
  if [[ "$(printf '%s\n' "1.37" "${ver_mm}" | sort -V | head -1)" != "1.37" ]]; then
    create_args+=(--enable-kubernetes-unstable-apis=certificates.k8s.io/v1beta1/podcertificaterequests,certificates.k8s.io/v1beta1/clustertrustbundles)
  fi

  gcloud container clusters create "${CLUSTER_NAME}" "${create_args[@]}" --quiet
  ok "Cluster ${CLUSTER_NAME} created"
fi

step "Fetching kubeconfig credentials for ${CLUSTER_NAME}"
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --zone "${CLUSTER_LOCATION}" \
  --project "${PROJECT_ID}" >/dev/null
ok "kubectl context: $(kubectl config current-context)"
