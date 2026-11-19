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

# Deletes the GKE cluster in `.env` after releasing all PersistentVolumeClaims
# so persistent disks do not linger and continue billing.
# Set YES=1 to skip confirmation; DELETE_ORPHAN_DISKS=true to delete any
# remaining `pvc-*` disks in PROJECT_ID.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

if [[ "${YES:-}" != "1" && "${YES:-}" != "true" ]]; then
  echo "This deletes GKE cluster '${CLUSTER_NAME}' (${CLUSTER_LOCATION}) in project '${PROJECT_ID}',"
  echo "including all workloads and PersistentVolumeClaim disks. It cannot be undone."
  read -r -p "Type the cluster name to continue: " answer || answer=""
  [[ "${answer}" == "${CLUSTER_NAME}" ]] || die "aborted"
fi

deletable_pvs() {
  kubectl get pv -o jsonpath='{range .items[?(@.spec.persistentVolumeReclaimPolicy=="Delete")]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true
}
no_deletable_pvs() {
  local pvs
  pvs="$(deletable_pvs)"
  [[ -z "${pvs}" ]]
}

if gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  step "Releasing WorkerPools, StatefulSets, and PersistentVolumeClaims in ${CLUSTER_NAME}"
  kubectl delete workerpool --all --all-namespaces --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n ate-system delete statefulset postgres --ignore-not-found --cascade=foreground --wait=true >/dev/null 2>&1 || true
  kubectl delete pvc --all --all-namespaces --wait=false >/dev/null 2>&1 || true
  if ( wait_for 300 "PersistentVolumes to be released" no_deletable_pvs ); then
    ok "PersistentVolumes released"
  else
    warn "some PersistentVolumes did not release in time; check disk list after cluster delete"
  fi
fi

if gcloud container clusters describe "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  step "Deleting GKE cluster ${CLUSTER_NAME} (${CLUSTER_LOCATION})..."
  gcloud container clusters delete "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --project "${PROJECT_ID}" \
    --quiet
  ok "Cluster ${CLUSTER_NAME} deleted"
else
  ok "Cluster ${CLUSTER_NAME} already absent"
fi

step "Checking for leftover pvc- disks in ${PROJECT_ID}"
orphan_disks="$(gcloud compute disks list --project "${PROJECT_ID}" --filter='name~^pvc-' --format='value(name,zone.basename())' 2>/dev/null || true)"
if [[ -n "${orphan_disks}" ]]; then
  if [[ "${DELETE_ORPHAN_DISKS:-false}" == "true" ]]; then
    while read -r disk_name disk_zone; do
      [[ -z "${disk_name}" ]] && continue
      warn "Deleting leftover PVC disk ${disk_name} (${disk_zone})"
      gcloud compute disks delete "${disk_name}" --zone="${disk_zone}" --project="${PROJECT_ID}" --quiet
    done <<< "${orphan_disks}"
    ok "Leftover PVC disks deleted"
  else
    warn "Leftover pvc- disks still present in ${PROJECT_ID} (re-run with DELETE_ORPHAN_DISKS=true or delete manually):"
    gcloud compute disks list --project "${PROJECT_ID}" --filter='name~^pvc-' --format='table(name,sizeGb,zone.basename())' >&2 || true
  fi
else
  ok "No leftover pvc- disks in ${PROJECT_ID}"
fi
