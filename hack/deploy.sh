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

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh ]]; then
  source .ate-dev-env.sh
fi

: "${PROJECT_ID:?PROJECT_ID must be set (see .ate-dev-env.sh.example)}"
: "${BUCKET_NAME:?BUCKET_NAME must be set (see .ate-dev-env.sh.example)}"
: "${KO_DOCKER_REPO:?KO_DOCKER_REPO must be set (see .ate-dev-env.sh.example)}"
SUBSTRATE_REPO="${SUBSTRATE_REPO:-$(cd "${ROOT}/../substrate" && pwd)}"
IDLE_TIMEOUT="${IDLE_TIMEOUT:-10m}"
WORKERPOOL_IDLE_TIMEOUT="${WORKERPOOL_IDLE_TIMEOUT:-2m}"
MAX_ACTIVE_WORLDS="${MAX_ACTIVE_WORLDS:-1}"
PREEMPT_IDLE_GRACE="${PREEMPT_IDLE_GRACE:-5s}"
BASE_UDP_PORT="${BASE_UDP_PORT:-34197}"
if [[ -n "${WORLDS_LIST:-}" ]]; then
  read -r -a WORLDS <<< "${WORLDS_LIST}"
else
  WORLDS=(world-vanilla world-deathworld world-railworld world-ribbonworld)
fi

ROUTES_ITEMS=()
GATEWAY_UDP_PORTS=""
for idx in "${!WORLDS[@]}"; do
  PORT=$((BASE_UDP_PORT + idx))
  ACTOR="${WORLDS[$idx]}"
  ROUTES_ITEMS+=("${PORT}=factorio/${ACTOR}")
  GATEWAY_UDP_PORTS="${GATEWAY_UDP_PORTS}        - name: udp-port-${PORT}
          containerPort: ${PORT}
          hostPort: ${PORT}
          protocol: UDP
"
done
ROUTES_ARG="$(IFS=,; echo "${ROUTES_ITEMS[*]}")"
export ROUTES_ARG GATEWAY_UDP_PORTS

KO_BIN="$(cd "${SUBSTRATE_REPO}" && ./hack/run-tool.sh --print-bin-path ko)"
run_ko() {
  "${KO_BIN}" "$@"
}

KUBECTL_ATE_BIN="/tmp/kubectl-ate-deploy"
(cd "${SUBSTRATE_REPO}" && go build -o "${KUBECTL_ATE_BIN}" ./cmd/kubectl-ate)
run_kubectl_ate() {
  "${KUBECTL_ATE_BIN}" "$@"
}

echo "==> Checking cluster atelet DaemonSet..."
if ! kubectl get ds -n ate-system atelet >/dev/null 2>&1; then
  echo "error: could not find atelet DaemonSet in namespace ate-system" >&2
  exit 1
fi
kubectl patch ds -n ate-system atelet \
  -p '{"spec":{"template":{"metadata":{"annotations":{"cluster-autoscaler.kubernetes.io/safe-to-evict":"true"}}}}}' >/dev/null 2>&1 || true
if ! kubectl get ds -n ate-system atelet -o jsonpath='{.spec.template.spec.containers[0].args[*]}' | grep -q -- '--snapshot-upload-delay='; then
  echo "    Configuring atelet background snapshot upload (2s) and detach (30s) delays for scale-to-zero..."
  kubectl patch ds -n ate-system atelet --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--snapshot-upload-delay=2s"},{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--snapshot-detach-delay=30s"}]'
  kubectl rollout status ds/atelet -n ate-system --timeout=120s
fi

echo "==> Building/resolving ateworker-gvisor worker image..."
WORKER_IMAGE="$(cd "${SUBSTRATE_REPO}" && ./hack/run-tool.sh ko build ./cmd/ateworker-gvisor)"
echo "    WORKER_IMAGE=${WORKER_IMAGE}"

echo "==> Ensuring GCP Firewall Rule allows UDP hostPorts (34197-34297)..."
if ! gcloud compute firewall-rules describe factorio-ate-allow-udp-hostport --project="${PROJECT_ID}" >/dev/null 2>&1; then
  echo "    Creating firewall rule factorio-ate-allow-udp-hostport..."
  gcloud compute firewall-rules create factorio-ate-allow-udp-hostport \
    --project="${PROJECT_ID}" \
    --network=default \
    --allow=udp:34197-34297 \
    --source-ranges=0.0.0.0/0 \
    --description="Allow Factorio UDP traffic to node hostPorts 34197-34297"
fi

if kubectl get svc -n factorio factorio-gateway >/dev/null 2>&1; then
  echo "==> Deleting legacy factorio-gateway LoadBalancer Service (using hostPort ingress)..."
  kubectl delete svc -n factorio factorio-gateway --ignore-not-found
fi

echo "==> Deploying factorio Namespace, WorkerPool, and UDP Gateway (IDLE_TIMEOUT=${IDLE_TIMEOUT}, WORKERPOOL_IDLE_TIMEOUT=${WORKERPOOL_IDLE_TIMEOUT}, MAX_ACTIVE_WORLDS=${MAX_ACTIVE_WORLDS}, WORLDS=${#WORLDS[@]})..."
export WORKER_IMAGE IDLE_TIMEOUT WORKERPOOL_IDLE_TIMEOUT MAX_ACTIVE_WORLDS PREEMPT_IDLE_GRACE
python3 -c '
import os, sys
tmpl = open("manifests/factorio-substrate.yaml.tmpl").read()
replacements = {
    "${WORKER_IMAGE}": os.environ["WORKER_IMAGE"],
    "${IDLE_TIMEOUT}": os.environ["IDLE_TIMEOUT"],
    "${WORKERPOOL_IDLE_TIMEOUT}": os.environ["WORKERPOOL_IDLE_TIMEOUT"],
    "${MAX_ACTIVE_WORLDS}": os.environ["MAX_ACTIVE_WORLDS"],
    "${PREEMPT_IDLE_GRACE}": os.environ["PREEMPT_IDLE_GRACE"],
    "${ROUTES_ARG}": os.environ["ROUTES_ARG"],
    "${GATEWAY_UDP_PORTS}": os.environ["GATEWAY_UDP_PORTS"].rstrip(),
}
for k, v in replacements.items():
    tmpl = tmpl.replace(k, v)
sys.stdout.write(tmpl)
' | run_ko resolve -f - \
  | kubectl apply -f -

echo "==> Ensuring WorkerPool has ${MAX_ACTIVE_WORLDS} replica(s) during deployment..."
if [[ "${RESIZE_NODEPOOL:-true}" == "true" ]] && [[ -n "${CLUSTER_NAME:-}" ]] && [[ -n "${CLUSTER_LOCATION:-}" ]]; then
  CURRENT_MAX_NODES="$(gcloud container node-pools describe substrate-worker-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" --format='value(autoscaling.maxNodeCount)' 2>/dev/null || echo "")"
  if [[ -n "${CURRENT_MAX_NODES}" && "${CURRENT_MAX_NODES}" != "${MAX_ACTIVE_WORLDS}" ]]; then
    echo "    Updating 'substrate-worker-pool' autoscaling max-nodes (${CURRENT_MAX_NODES} -> ${MAX_ACTIVE_WORLDS})..."
    gcloud container node-pools update substrate-worker-pool \
      --cluster "${CLUSTER_NAME}" \
      --zone "${CLUSTER_LOCATION}" \
      --project "${PROJECT_ID}" \
      --enable-autoscaling \
      --min-nodes=0 \
      --max-nodes="${MAX_ACTIVE_WORLDS}" \
      --quiet || true
  fi
fi
kubectl patch workerpool factorio -n factorio --type=merge -p "{\"spec\":{\"replicas\":${MAX_ACTIVE_WORLDS}}}" || true

echo "==> Waiting for WorkerPool and UDP Gateway to roll out..."
kubectl wait --for=create deployment/factorio -n factorio --timeout=120s
kubectl rollout status deployment/factorio -n factorio --timeout=300s
kubectl rollout status deployment/factorio-gateway -n factorio --timeout=300s

echo "==> Ensuring 'factorio' Atespace exists..."
if ! run_kubectl_ate get atespace factorio >/dev/null 2>&1; then
  run_kubectl_ate create atespace factorio
fi

echo "==> Resolving latest factoriotools/factorio:${FACTORIO_TAG:-stable} digest from Docker Hub..."
FACTORIO_TAG="${FACTORIO_TAG:-stable}"
FACTORIO_DIGEST="$(curl -fsSL "https://hub.docker.com/v2/repositories/factoriotools/factorio/tags/${FACTORIO_TAG}" 2>/dev/null | jq -r '.digest // empty' || true)"
if [[ -z "${FACTORIO_DIGEST}" ]]; then
  FACTORIO_DIGEST="sha256:0ef394757b906f8e2e42d7e1f05332ad7cf7575c40d9b2412fa4c477189bdea7"
fi
FACTORIO_IMAGE="${FACTORIO_IMAGE:-index.docker.io/factoriotools/factorio@${FACTORIO_DIGEST}}"
echo "    FACTORIO_IMAGE=${FACTORIO_IMAGE}"

echo "==> Rendering 'factorio-server' ActorTemplate with ko..."
RENDERED_TEMPLATE="$(sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
    -e "s|\${FACTORIO_IMAGE}|${FACTORIO_IMAGE}|g" \
    manifests/factorio-template.yaml.tmpl \
  | run_ko resolve -f -)"
NEW_SIDECAR_IMAGE="$(echo "${RENDERED_TEMPLATE}" | grep -A2 'name: sidecar' | awk '/image:/ {print $2}')"

if [[ "${RESET_WORLDS:-false}" == "true" ]]; then
  echo "==> RESET_WORLDS=true: suspending and deleting existing world Actors so they generate fresh custom maps on next connect..."
  for ACTOR in "${WORLDS[@]}"; do
    kubectl create --raw "/api/v1/namespaces/factorio/services/factorio-gateway-http:status-http/proxy/suspend?actor=factorio/${ACTOR}" -f - <<< '{}' >/dev/null 2>&1 || true
    run_kubectl_ate suspend actor "${ACTOR}" -a factorio >/dev/null 2>&1 || true
    run_kubectl_ate delete actor "${ACTOR}" -a factorio >/dev/null 2>&1 || true
  done
fi

echo "==> Checking 'factorio-server' ActorTemplate..."
if run_kubectl_ate get actor-template factorio-server -a factorio >/dev/null 2>&1; then
  TMPL_JSON="$(run_kubectl_ate get actor-template factorio-server -a factorio -o json)"
  SNAP_URI="$(echo "${TMPL_JSON}" | jq -r '.status.goldenSnapshotStatus.goldenTag.name // empty')"
  CUR_FACTORIO_IMAGE="$(echo "${TMPL_JSON}" | jq -r '.containers[] | select(.name=="factorio") | .image // empty')"
  CUR_SIDECAR_IMAGE="$(echo "${TMPL_JSON}" | jq -r '.containers[] | select(.name=="sidecar") | .image // empty')"
  if [[ -z "${SNAP_URI}" || "${CUR_FACTORIO_IMAGE}" != "${FACTORIO_IMAGE}" || "${CUR_SIDECAR_IMAGE}" != "${NEW_SIDECAR_IMAGE}" || "${RECREATE_TEMPLATE:-false}" == "true" ]]; then
    OLD_UID="$(echo "${TMPL_JSON}" | jq -r '.metadata.uid // empty')"
    echo "    Recreating factorio-server ActorTemplate (factorio=${FACTORIO_IMAGE}, sidecar=${NEW_SIDECAR_IMAGE})..."
    for ACTOR in "${WORLDS[@]}"; do
      run_kubectl_ate suspend actor "${ACTOR}" -a factorio >/dev/null 2>&1 || true
      run_kubectl_ate delete actor "${ACTOR}" -a factorio >/dev/null 2>&1 || true
    done
    run_kubectl_ate delete actor-template factorio-server -a factorio || true
    if [[ -n "${OLD_UID}" ]]; then
      kubectl exec -n ate-system postgres-0 -c postgres -- psql -U postgres -d atepg -c "DELETE FROM worker_assignments; DELETE FROM workers; DELETE FROM actors WHERE name = '${OLD_UID}';" >/dev/null 2>&1 || true
      kubectl delete pod -n factorio -l ate.dev/worker-pool=factorio --wait=false >/dev/null 2>&1 || true
      kubectl rollout status deployment/factorio -n factorio --timeout=120s
    fi
    sleep 2
  fi
fi
if ! run_kubectl_ate get actor-template factorio-server -a factorio >/dev/null 2>&1; then
  echo "${RENDERED_TEMPLATE}" | run_kubectl_ate create actor-template -f -
fi

echo "==> Waiting for 'factorio-server' ActorTemplate Golden Snapshot to become Ready..."
for i in {1..90}; do
  STATUS="$(run_kubectl_ate get actor-template factorio-server -a factorio -o json 2>/dev/null || true)"
  if echo "${STATUS}" | grep -q '"goldenTag"'; then
    echo "    Golden Snapshot Ready!"
    break
  fi
  if [[ $i -eq 90 ]]; then
    echo "error: timed out waiting for Golden Snapshot" >&2
    echo "${STATUS}" >&2
    exit 1
  fi
  sleep 5
done

echo "==> Creating Factorio world Actors (${#WORLDS[@]} worlds)..."
EXISTING_ACTORS="$(run_kubectl_ate get actors -a factorio -o json 2>/dev/null | jq -r '.actors[]?.metadata.name // empty' || true)"
if [[ "${PRUNE_EXTRA_WORLDS:-true}" == "true" && -n "${EXISTING_ACTORS}" ]]; then
  WANT_ACTORS="$(printf "%s\n" "${WORLDS[@]}")"
  PRUNE_BATCH=0
  for EXISTING in ${EXISTING_ACTORS}; do
    if ! grep -qx "${EXISTING}" <<< "${WANT_ACTORS}"; then
      echo "    Pruning extra Actor '${EXISTING}' not in target WORLDS list..."
      (
        run_kubectl_ate suspend actor "${EXISTING}" -a factorio >/dev/null 2>&1 || true
        run_kubectl_ate delete actor "${EXISTING}" -a factorio >/dev/null 2>&1 || true
      ) &
      PRUNE_BATCH=$((PRUNE_BATCH + 1))
      if [[ "${PRUNE_BATCH}" -ge 8 ]]; then
        wait
        PRUNE_BATCH=0
      fi
    fi
  done
  wait
fi
BATCH=0
for ACTOR in "${WORLDS[@]}"; do
  if ! grep -qx "${ACTOR}" <<< "${EXISTING_ACTORS}"; then
    run_kubectl_ate create actor "${ACTOR}" -a factorio --template factorio-server >/dev/null &
    BATCH=$((BATCH + 1))
    if [[ "${BATCH}" -ge 8 ]]; then
      wait
      BATCH=0
    fi
  fi
done
wait

echo "==> Current Actors in 'factorio' atespace:"
run_kubectl_ate get actor -a factorio

GW_NODE="$(kubectl get pod -n factorio -l app=factorio-gateway -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
EXTERNAL_IP=""
if [[ -n "${GW_NODE}" ]]; then
  EXTERNAL_IP="$(kubectl get node "${GW_NODE}" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)"
fi

echo "================================================================================"
echo " Factorio on Agent Substrate Deployed (Spot Nodes + hostPort Ingress)!"
echo " Gateway Node:        ${GW_NODE:-<pending>}"
echo " External Host IP:    ${EXTERNAL_IP:-<pending>}"
echo ""
echo " Active World Routes (UDP hostPorts):"
for idx in "${!WORLDS[@]}"; do
  PORT=$((BASE_UDP_PORT + idx))
  ACTOR="${WORLDS[$idx]}"
  echo "   - ${EXTERNAL_IP:-<NODE_IP>}:${PORT}  ==>  factorio/${ACTOR}"
done
echo "================================================================================"
