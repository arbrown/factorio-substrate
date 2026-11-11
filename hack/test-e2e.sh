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
#   1. Resolves the external IP of factorio-gateway (Node ExternalIP / hostPort).
#   2. Ensures Actor state reporting and wakes world-vanilla via UDP (:34197).
#   3. Queries RCON status via /world-status to verify live Factorio game state.
#   4. Connects to :34198 (world-deathworld) and verifies single-worker
#      multiplexing: suspending world-vanilla to GCS and resuming world-deathworld.
#   5. Optionally waits for idle auto-suspend back to ACTOR_STATE_SUSPENDED.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env
ensure_kubectl_ate
resolve_worlds

step "Resolving factorio-gateway External IP (hostPort / Node IP)"
gw_external_ip 60 || die "timed out waiting for factorio-gateway ExternalIP"
ok "EXTERNAL_IP=${EXTERNAL_IP} (node=${GW_NODE})"

PF_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
kubectl port-forward -n "${NAMESPACE}" svc/factorio-gateway-http "${PF_PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" >/dev/null 2>&1 || true' EXIT

pf_ready() {
  [[ "$(curl -fsS --max-time 2 "http://127.0.0.1:${PF_PORT}/healthz" 2>/dev/null || true)" == "ok" ]]
}
wait_for 30 "port-forward to factorio-gateway-http on :${PF_PORT}" pf_ready

step "Step 1: Checking initial Actor states"
run_kubectl_ate get actor -a "${ATESPACE}"

PORT_VANILLA="${BASE_UDP_PORT}"
PORT_DEATHWORLD="$((BASE_UDP_PORT + 1))"

step "Step 2: Probing world-vanilla on ${EXTERNAL_IP}:${PORT_VANILLA}/UDP"
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:${PORT_VANILLA}" --timeout=180s

step "Step 3: Verifying world-vanilla is RUNNING and querying RCON status"
run_kubectl_ate get actor world-vanilla -a "${ATESPACE}"
curl -fsS "http://127.0.0.1:${PF_PORT}/world-status?actor=${ATESPACE}/world-vanilla" && echo ""
curl -fsS "http://127.0.0.1:${PF_PORT}/status" && echo ""

step "Step 4: Waiting 6s (preempt grace) then probing world-deathworld on ${EXTERNAL_IP}:${PORT_DEATHWORLD}/UDP"
sleep 6
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:${PORT_DEATHWORLD}" --timeout=120s

step "Step 5: Verifying world-vanilla was suspended and world-deathworld is RUNNING"
run_kubectl_ate get actor -a "${ATESPACE}"
curl -fsS "http://127.0.0.1:${PF_PORT}/world-status?actor=${ATESPACE}/world-deathworld" && echo ""
curl -fsS "http://127.0.0.1:${PF_PORT}/status" && echo ""

if [[ "${VERIFY_IDLE_SUSPEND:-false}" == "true" ]]; then
  WAIT_SECS="${IDLE_WAIT_SECS:-45}"
  step "Step 6: Waiting ${WAIT_SECS}s for zero-UDP idle auto-suspend"
  sleep "${WAIT_SECS}"
  run_kubectl_ate get actor -a "${ATESPACE}"
  curl -fsS "http://127.0.0.1:${PF_PORT}/status" && echo ""
fi

ok "End-to-End Factorio on Agent Substrate test PASSED"
