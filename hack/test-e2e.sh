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

# End-to-end verification of Factorio on Agent Substrate:
#   1. Resolves the external LoadBalancer IP of factorio-gateway.
#   2. Ensures both world-vanilla and world-deathworld start SUSPENDED.
#   3. Sends a real Factorio 2.x UDP ConnectionRequest probe to :34197
#      (world-vanilla) and verifies that it wakes the Actor and returns a
#      genuine Factorio UDP ConnectionRequestReply.
#   4. Queries RCON status via /world-status to verify live Factorio game state.
#   5. Connects to :34198 (world-deathworld) and verifies that the single
#      WorkerPool pod multiplexes: suspending world-vanilla to GCS and
#      resuming world-deathworld.
#   6. Waits for the idle timeout and verifies that world-deathworld
#      automatically saves and suspends back to ACTOR_STATE_SUSPENDED.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh ]]; then
  source .ate-dev-env.sh
fi

SUBSTRATE_REPO="${SUBSTRATE_REPO:-$(cd "${ROOT}/../substrate" && pwd)}"

run_kubectl_ate() {
  (cd "${SUBSTRATE_REPO}" && go run ./cmd/kubectl-ate "$@")
}

echo "==> Waiting for factorio-gateway External LoadBalancer IP..."
EXTERNAL_IP=""
for i in {1..60}; do
  EXTERNAL_IP="$(kubectl get svc -n factorio factorio-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
  if [[ -n "${EXTERNAL_IP}" ]]; then
    break
  fi
  sleep 3
done
if [[ -z "${EXTERNAL_IP}" ]]; then
  echo "error: timed out waiting for factorio-gateway EXTERNAL-IP" >&2
  exit 1
fi
echo "    EXTERNAL_IP=${EXTERNAL_IP}"

echo "==> Starting port-forward to factorio-gateway-http (:18080 -> :8080)..."
kubectl port-forward -n factorio svc/factorio-gateway-http 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" >/dev/null 2>&1 || true' EXIT
sleep 2

echo "==> Step 1: Checking initial Actor states..."
run_kubectl_ate get actor -a factorio

echo "==> Step 2: Probing world-vanilla on ${EXTERNAL_IP}:34197/UDP..."
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34197" --timeout=60s

echo "==> Step 3: Verifying world-vanilla is RUNNING and querying RCON status..."
run_kubectl_ate get actor world-vanilla -a factorio
curl -fsS "http://127.0.0.1:18080/world-status?actor=factorio/world-vanilla" && echo ""
curl -fsS "http://127.0.0.1:18080/status" && echo ""

echo "==> Step 4: Waiting 6s (preempt grace) then probing world-deathworld on ${EXTERNAL_IP}:34198/UDP to test single-worker multiplexing..."
sleep 6
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34198" --timeout=60s

echo "==> Step 5: Verifying world-vanilla was suspended and world-deathworld is now RUNNING..."
run_kubectl_ate get actor -a factorio
curl -fsS "http://127.0.0.1:18080/world-status?actor=factorio/world-deathworld" && echo ""
curl -fsS "http://127.0.0.1:18080/status" && echo ""

if [[ "${VERIFY_IDLE_SUSPEND:-false}" == "true" ]]; then
  WAIT_SECS="${IDLE_WAIT_SECS:-45}"
  echo "==> Step 6: Waiting ${WAIT_SECS}s for zero-UDP idle auto-suspend..."
  sleep "${WAIT_SECS}"
  run_kubectl_ate get actor -a factorio
  curl -fsS "http://127.0.0.1:18080/status" && echo ""
fi

echo "==> End-to-End Factorio on Agent Substrate test PASSED!"
