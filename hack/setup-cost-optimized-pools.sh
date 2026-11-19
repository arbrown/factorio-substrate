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

# Migrates the GKE cluster to a cost-optimized Option A scale-to-zero topology:
#   1. Single-node Spot `control-pool` (e2-medium) in CLUSTER_LOCATION.
#   2. Compact 10Gi / 5m-CPU `ate-system/postgres` StatefulSet (preserving roles + data).
#   3. Deletes the initial `e2-standard-4` bootstrap pool (`default-pool`).
#   4. Enables multi-zone worker scheduling (`WORKER_ZONES`) and applies `manifests/computeclass.yaml`.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

echo "================================================================================"
echo " Cost-Optimized Node Pools & Multi-Zone Spot ComputeClass"
echo " Cluster: ${CLUSTER_NAME} (${CLUSTER_LOCATION}) in ${PROJECT_ID}"
echo "================================================================================"

kubectl get ds -n ate-system atelet >/dev/null 2>&1 \
  || die "atelet DaemonSet not found in ate-system — run 'make install-substrate' first"

step "Setting cluster autoscaling profile to optimize-utilization"
cur_autoscaling_profile="$(gcloud container clusters describe "${CLUSTER_NAME}" \
  --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" \
  --format='value(autoscaling.autoscalingProfile)' 2>/dev/null || true)"
if [[ "${cur_autoscaling_profile}" != "OPTIMIZE_UTILIZATION" ]]; then
  gcloud container clusters update "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --project "${PROJECT_ID}" \
    --autoscaling-profile=optimize-utilization \
    --quiet >/dev/null
  ok "Autoscaling profile set"
else
  ok "Autoscaling profile already OPTIMIZE_UTILIZATION"
fi

release_channel="$(gcloud container clusters describe "${CLUSTER_NAME}" \
  --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" \
  --format='value(releaseChannel.channel)' 2>/dev/null || true)"

create_control_pool() {
  local args=(
    --cluster="${CLUSTER_NAME}"
    --zone="${CLUSTER_LOCATION}"
    --project="${PROJECT_ID}"
    --spot
    --machine-type="${CONTROL_POOL_MACHINE_TYPE}"
    --disk-size=30GB
    --disk-type=pd-standard
    --num-nodes=1
    --node-locations="${CLUSTER_LOCATION}"
    --tags="${CLUSTER_NODE_TAGS}"
  )
  if [[ -z "${release_channel}" ]]; then
    args+=(--no-enable-autoupgrade)
  fi
  gcloud container node-pools create "${CONTROL_POOL_NAME}" "${args[@]}" --quiet
}

step "Ensuring Spot ${CONTROL_POOL_NAME} (1x ${CONTROL_POOL_MACHINE_TYPE} in ${CLUSTER_LOCATION})"
if gcloud container node-pools describe "${CONTROL_POOL_NAME}" \
    --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  control_is_spot="$(gcloud container node-pools describe "${CONTROL_POOL_NAME}" \
    --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" \
    --format='value(config.spot)' 2>/dev/null || true)"
  if [[ "${control_is_spot}" != "True" ]]; then
    warn "Existing '${CONTROL_POOL_NAME}' is not Spot; recreating as Spot"
    gcloud container node-pools delete "${CONTROL_POOL_NAME}" \
      --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" --quiet
    create_control_pool
  else
    gcloud container node-pools update "${CONTROL_POOL_NAME}" \
      --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" \
      --node-locations="${CLUSTER_LOCATION}" --quiet >/dev/null || true
    ok "Spot ${CONTROL_POOL_NAME} already exists"
  fi
else
  create_control_pool
  ok "Created Spot ${CONTROL_POOL_NAME}"
fi
kubectl wait --for=condition=Ready node -l "cloud.google.com/gke-nodepool=${CONTROL_POOL_NAME}" --timeout=180s >/dev/null

# Cordon initial bootstrap pools so migrated workloads schedule directly onto CONTROL_POOL_NAME.
for legacy_pool in default-pool substrate-node-pool; do
  if gcloud container node-pools describe "${legacy_pool}" \
      --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
    for node in $(kubectl get nodes -l "cloud.google.com/gke-nodepool=${legacy_pool}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true); do
      kubectl cordon "${node}" >/dev/null || true
    done
  fi
done

step "Right-sizing ate-system control plane requests for ${CONTROL_POOL_MACHINE_TYPE}"
kubectl patch ds -n ate-system atelet \
  -p '{"spec":{"template":{"metadata":{"annotations":{"cluster-autoscaler.kubernetes.io/safe-to-evict":"true"}}}}}' >/dev/null || true
atelet_patch="$(kubectl get ds -n ate-system atelet -o json | jq -c '
  [
    (.spec.template.spec.containers // [] | to_entries[] | select(.value.resources.requests != null) |
      {op: "replace", path: "/spec/template/spec/containers/\(.key)/resources/requests/cpu", value: "5m"},
      {op: "replace", path: "/spec/template/spec/containers/\(.key)/resources/requests/memory", value: "64Mi"}
    ),
    (.spec.template.spec.initContainers // [] | to_entries[] | select(.value.resources.requests != null) |
      {op: "replace", path: "/spec/template/spec/initContainers/\(.key)/resources/requests/cpu", value: "5m"},
      {op: "replace", path: "/spec/template/spec/initContainers/\(.key)/resources/requests/memory", value: "64Mi"}
    )
  ]')"
[[ "${atelet_patch}" != "[]" ]] && kubectl patch ds -n ate-system atelet --type=json -p="${atelet_patch}" >/dev/null

if ! kubectl get ds -n ate-system atelet -o jsonpath='{.spec.template.spec.containers[0].args[*]}' 2>/dev/null | grep -q -- '--snapshot-upload-delay='; then
  kubectl patch ds -n ate-system atelet --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--snapshot-upload-delay=2s"},{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--snapshot-detach-delay=30s"}]' >/dev/null || true
fi

if kubectl get deployment -n ate-system atenet-egress >/dev/null 2>&1; then
  egress_patch="$(kubectl get deployment -n ate-system atenet-egress -o json | jq -c '
    [
      (.spec.template.spec.containers // [] | to_entries[] | select(.value.resources.requests != null) |
        {op: "replace", path: "/spec/template/spec/containers/\(.key)/resources/requests/cpu", value: "5m"},
        {op: "replace", path: "/spec/template/spec/containers/\(.key)/resources/requests/memory", value: "128Mi"}
      ),
      (.spec.template.spec.initContainers // [] | to_entries[] | select(.value.resources.requests != null) |
        {op: "replace", path: "/spec/template/spec/initContainers/\(.key)/resources/requests/cpu", value: "5m"},
        {op: "replace", path: "/spec/template/spec/initContainers/\(.key)/resources/requests/memory", value: "128Mi"}
      )
    ]')"
  [[ "${egress_patch}" != "[]" ]] && kubectl patch deployment -n ate-system atenet-egress --type=json -p="${egress_patch}" >/dev/null
fi
kubectl patch deployment -n kube-system konnectivity-agent --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/tolerations/-","value":{"key":"ate.dev/sandboxClass","operator":"Exists"}}]' >/dev/null 2>&1 || true

kubectl scale deployment/ate-api-server -n ate-system --replicas=1 >/dev/null
kubectl scale deployment/ate-idp-server -n ate-system --replicas=1 >/dev/null 2>&1 || true
ok "Control-plane resource requests tuned"

# Replace 500Gi / 2-CPU Postgres with compact 10Gi / 5m-CPU StatefulSet while preserving roles and data.
cur_storage="$(kubectl get pvc -n ate-system data-postgres-0 -o jsonpath='{.spec.resources.requests.storage}' 2>/dev/null || true)"
cur_cpu="$(kubectl get sts -n ate-system postgres -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}' 2>/dev/null || true)"
if [[ "${cur_storage}" != "10Gi" || "${cur_cpu}" == "2" ]]; then
  step "Compacting ate-system/postgres (${cur_storage:-unknown} -> 10Gi, cpu ${cur_cpu:-unknown} -> 5m)"
  backup_dir="$(mktemp -d "${TMPDIR:-/tmp}/factorio-pg-backup.XXXXXX")"
  trap 'rm -rf "${backup_dir}"' EXIT

  kubectl scale deployment/ate-api-server -n ate-system --replicas=0 >/dev/null
  kubectl wait --for=delete pod -l app=ate-api-server -n ate-system --timeout=120s >/dev/null 2>&1 || true

  kubectl exec -n ate-system postgres-0 -c postgres -- pg_dumpall -U postgres --roles-only \
    | grep -vE '^(CREATE|ALTER) ROLE postgres\b' > "${backup_dir}/roles.sql"
  kubectl exec -n ate-system postgres-0 -c postgres -- pg_dump -U postgres atepg \
    > "${backup_dir}/atepg.sql"
  [[ -s "${backup_dir}/atepg.sql" ]] || die "postgres atepg backup is empty; aborting before storage delete"
  ok "Backed up roles ($(wc -l < "${backup_dir}/roles.sql") lines) and atepg ($(wc -l < "${backup_dir}/atepg.sql") lines)"

  kubectl delete sts -n ate-system postgres --wait=true
  kubectl delete pvc -n ate-system data-postgres-0 --wait=true
  no_pg_pv() {
    local pv
    pv="$(kubectl get pv -o jsonpath='{range .items[?(@.spec.claimRef.name=="data-postgres-0")]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
    [[ -z "${pv}" ]]
  }
  wait_for 180 "postgres PersistentVolume release" no_pg_pv

  kubectl apply -f "${REPO_ROOT}/manifests/postgres-compact.yaml"
  kubectl wait --for=condition=ready pod/postgres-0 -n ate-system --timeout=300s
  atepg_ready() {
    kubectl exec -n ate-system postgres-0 -c postgres -- psql -U postgres -d atepg -Atc 'SELECT 1' >/dev/null 2>&1
  }
  wait_for 60 "postgres atepg database initialization" atepg_ready

  if [[ -s "${backup_dir}/roles.sql" ]]; then
    kubectl exec -i -n ate-system postgres-0 -c postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d postgres < "${backup_dir}/roles.sql" >/dev/null
  fi
  kubectl exec -i -n ate-system postgres-0 -c postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d atepg < "${backup_dir}/atepg.sql" >/dev/null

  kubectl scale deployment/ate-api-server -n ate-system --replicas=1 >/dev/null
  kubectl rollout status deployment/ate-api-server -n ate-system --timeout=180s
  ok "Compact postgres-0 restored and ate-api-server Ready"
else
  kubectl apply -f "${REPO_ROOT}/manifests/postgres-compact.yaml" >/dev/null
  ok "ate-system/postgres already compact (${cur_storage}, cpu=${cur_cpu})"
fi

# Delete initial / legacy pools BEFORE adding multi-zone cluster locations so bootstrap VMs never replicate across zones.
for legacy_pool in default-pool substrate-node-pool substrate-worker-pool; do
  if gcloud container node-pools describe "${legacy_pool}" \
      --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
    step "Draining and deleting bootstrap/legacy pool '${legacy_pool}'"
    for node in $(kubectl get nodes -l "cloud.google.com/gke-nodepool=${legacy_pool}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true); do
      kubectl cordon "${node}" >/dev/null 2>&1 || true
      kubectl drain "${node}" --delete-emptydir-data --ignore-daemonsets --force --timeout=180s >/dev/null 2>&1 || true
    done
    gcloud container node-pools delete "${legacy_pool}" \
      --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" --quiet
    ok "Deleted pool '${legacy_pool}'"
  fi
done

_sort_zones() {
  printf '%s\n' "$1" | tr ';,' '\n' | tr -s '[:space:]' '\n' | grep -v '^$' | sort -u | paste -sd, -
}

step "Enabling multi-zone worker locations (${WORKER_ZONES}) on cluster ${CLUSTER_NAME}"
cur_cluster_locs="$(_sort_zones "$(gcloud container clusters describe "${CLUSTER_NAME}" \
  --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" --format='value(locations)' 2>/dev/null || true)")"
want_worker_zones="$(_sort_zones "${WORKER_ZONES}")"
if [[ "${cur_cluster_locs}" != "${want_worker_zones}" ]]; then
  gcloud container clusters update "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --project "${PROJECT_ID}" \
    --node-locations="${WORKER_ZONES}" \
    --quiet >/dev/null
  ok "Cluster locations set to ${WORKER_ZONES}"
else
  ok "Cluster locations already ${cur_cluster_locs}"
fi

step "Pinning ${CONTROL_POOL_NAME} to single zone (${CLUSTER_LOCATION})"
cur_pool_locs="$(_sort_zones "$(gcloud container node-pools describe "${CONTROL_POOL_NAME}" \
  --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" \
  --format='value(locations)' 2>/dev/null || true)")"
want_pool_locs="$(_sort_zones "${CLUSTER_LOCATION}")"
if [[ "${cur_pool_locs}" != "${want_pool_locs}" ]]; then
  gcloud container node-pools update "${CONTROL_POOL_NAME}" \
    --cluster "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --project "${PROJECT_ID}" \
    --node-locations="${CLUSTER_LOCATION}" \
    --quiet >/dev/null
  ok "${CONTROL_POOL_NAME} pinned to ${CLUSTER_LOCATION}"
else
  ok "${CONTROL_POOL_NAME} already pinned to ${cur_pool_locs}"
fi

step "Applying ComputeClass from manifests/computeclass.yaml"
render_computeclass_yaml | kubectl apply -f -

ensure_udp_firewall_rule

"${REPO_ROOT}/hack/observability.sh" apply

step "Waiting for ate-system workloads after node pool migration"
kubectl -n ate-system wait --for=condition=Available deploy --all --timeout=5m
kubectl -n ate-system rollout status sts/postgres --timeout=5m
kubectl -n ate-system rollout status ds/atelet --timeout=5m

echo "================================================================================"
echo " Cost-optimized pool migration complete"
kubectl get nodes -o wide
echo "================================================================================"
