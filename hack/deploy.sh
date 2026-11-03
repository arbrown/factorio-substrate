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
WORLDS=(world-vanilla world-deathworld world-railworld world-ribbonworld)

KO_BIN="$(cd "${SUBSTRATE_REPO}" && ./hack/run-tool.sh --print-bin-path ko)"
run_ko() {
  "${KO_BIN}" "$@"
}

run_kubectl_ate() {
  (cd "${SUBSTRATE_REPO}" && go run ./cmd/kubectl-ate "$@")
}

echo "==> Resolving Substrate version from cluster atelet DaemonSet..."
SUBSTRATE_VERSION="$(kubectl get ds -n ate-system -l app=atelet -o jsonpath='{.items[0].metadata.labels.ate\.dev/substrate-version}')"
if [[ -z "${SUBSTRATE_VERSION}" ]]; then
  echo "error: could not find atelet DaemonSet in namespace ate-system" >&2
  exit 1
fi
echo "    SUBSTRATE_VERSION=${SUBSTRATE_VERSION}"

echo "==> Building/resolving ateom-gvisor worker image..."
WORKER_IMAGE="$(cd "${SUBSTRATE_REPO}" && ./hack/run-tool.sh ko build ./cmd/ateom-gvisor)"
echo "    WORKER_IMAGE=${WORKER_IMAGE}"

echo "==> Deploying factorio Namespace, WorkerPool, and UDP Gateway (IDLE_TIMEOUT=${IDLE_TIMEOUT})..."
sed -e "s|\${SUBSTRATE_VERSION}|${SUBSTRATE_VERSION}|g" \
    -e "s|\${WORKER_IMAGE}|${WORKER_IMAGE}|g" \
    -e "s|\${IDLE_TIMEOUT}|${IDLE_TIMEOUT}|g" \
    manifests/factorio-substrate.yaml.tmpl \
  | run_ko resolve -f - \
  | kubectl apply -f -

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
  SNAP_URI="$(echo "${TMPL_JSON}" | jq -r '.status.goldenSnapshotStatus.goldenSnapshot.snapshotUri // empty')"
  CUR_FACTORIO_IMAGE="$(echo "${TMPL_JSON}" | jq -r '.containers[] | select(.name=="factorio") | .image // empty')"
  CUR_SIDECAR_IMAGE="$(echo "${TMPL_JSON}" | jq -r '.containers[] | select(.name=="sidecar") | .image // empty')"
  if [[ -z "${SNAP_URI}" || "${CUR_FACTORIO_IMAGE}" != "${FACTORIO_IMAGE}" || "${CUR_SIDECAR_IMAGE}" != "${NEW_SIDECAR_IMAGE}" || "${RECREATE_TEMPLATE:-false}" == "true" ]]; then
    OLD_UID="$(echo "${TMPL_JSON}" | jq -r '.metadata.uid // empty')"
    echo "    Recreating factorio-server ActorTemplate (factorio=${FACTORIO_IMAGE}, sidecar=${NEW_SIDECAR_IMAGE})..."
    for ACTOR in "${WORLDS[@]}"; do
      if [[ "${RESET_WORLDS:-false}" == "true" ]]; then
        run_kubectl_ate delete actor "${ACTOR}" -a factorio >/dev/null 2>&1 || true
      else
        run_kubectl_ate suspend actor "${ACTOR}" -a factorio >/dev/null 2>&1 || true
      fi
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
  if echo "${STATUS}" | grep -q '"snapshotUri"'; then
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

echo "==> Creating Factorio world Actors (${WORLDS[*]})..."
for ACTOR in "${WORLDS[@]}"; do
  if ! run_kubectl_ate get actor "${ACTOR}" -a factorio >/dev/null 2>&1; then
    run_kubectl_ate create actor "${ACTOR}" -a factorio --template factorio-server
  fi
done

echo "==> Current Actors in 'factorio' atespace:"
run_kubectl_ate get actor -a factorio

echo "==> Factorio Gateway LoadBalancer Service:"
kubectl get svc -n factorio factorio-gateway
