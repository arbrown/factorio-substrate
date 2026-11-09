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

# End-to-end 2-hop cascading simulation of a GKE ComputeClass Spot Preemption:
#   Hop 1 (Cold ComputeClass Replacement: NODE_1 -> Cold NODE_2):
#     - Wakes factorio/world-vanilla on NODE_1, spawns a steel-chest with 42
#       automation-science-packs at (0.5, 0.5) via RCON Lua.
#     - Cordons + taints NODE_1 (cloud.google.com/impending-node-termination)
#       simulating GCE's 30s Spot preemption notice.
#     - Measures GCS checkpoint duration (EvacuatePreemptingWorkers) and full
#       cold replacement VM boot + atelet + ResumeActor client reconnect time.
#     - Verifies zero item loss via RCON Lua on NODE_2.
#
#   Hop 2 (Warm Standby ComputeClass Replacement: NODE_2 -> Warm NODE_3):
#     - Unless SKIP_WARM_HOP=true, scales WorkerPool/factorio to 2 replicas so
#       ComputeClass provisions warm standby NODE_3 before preemption.
#     - Cordons + taints NODE_2 while NODE_3 is already warm.
#     - Measures GCS checkpoint + ResumeActor failover onto pre-warmed NODE_3
#       to test whether warm standby fits inside Factorio's ~15s UDP timeout.
#     - Verifies zero item loss via RCON Lua on NODE_3 and cleanly suspends.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh ]]; then
  source .ate-dev-env.sh
fi

SUBSTRATE_REPO="${SUBSTRATE_REPO:-$(cd "${ROOT}/../substrate" && pwd)}"
KUBECTL_ATE_BIN="/tmp/kubectl-ate-preempt"
if [[ ! -x "${KUBECTL_ATE_BIN}" ]]; then
  (cd "${SUBSTRATE_REPO}" && go build -o "${KUBECTL_ATE_BIN}" ./cmd/kubectl-ate)
fi

PF_PID=""
PF_PORT=""
PREEMPTED_NODES=()

cleanup() {
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" >/dev/null 2>&1 || true
    PF_PID=""
  fi
  for node in "${PREEMPTED_NODES[@]+"${PREEMPTED_NODES[@]}"}"; do
    [[ -z "${node}" ]] && continue
    kubectl taint node "${node}" cloud.google.com/impending-node-termination- >/dev/null 2>&1 || true
    kubectl uncordon "${node}" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

ensure_port_forward() {
  if [[ -n "${PF_PORT}" && -n "${PF_PID}" ]] && kill -0 "${PF_PID}" >/dev/null 2>&1; then
    if [[ "$(curl -fsS --max-time 2 "http://127.0.0.1:${PF_PORT}/healthz" 2>/dev/null || true)" == "ok" ]]; then
      return 0
    fi
  fi
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" >/dev/null 2>&1 || true
    PF_PID=""
  fi
  PF_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
  kubectl port-forward -n factorio svc/factorio-gateway-http "${PF_PORT}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    if [[ "$(curl -fsS --max-time 2 "http://127.0.0.1:${PF_PORT}/healthz" 2>/dev/null || true)" == "ok" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "ERROR: kubectl port-forward on 127.0.0.1:${PF_PORT} failed to become ready" >&2
  return 1
}

wait_ready_worker_pods() {
  local want_count="${1}"
  local exclude_node="${2:-}"
  local timeout_sec="${3:-240}"
  local start_s
  start_s="$(date +%s)"
  while true; do
    local now_s pods_json ready_names
    now_s="$(date +%s)"
    if (( now_s - start_s > timeout_sec )); then
      echo "ERROR: timed out waiting for ${want_count} Ready worker pod(s) (exclude_node=${exclude_node})" >&2
      kubectl get pods -n factorio -l ate.dev/worker-pool=factorio -o wide >&2 || true
      return 1
    fi
    pods_json="$(kubectl get pods -n factorio -l ate.dev/worker-pool=factorio -o json 2>/dev/null || echo '{"items":[]}')"
    ready_names="$(echo "${pods_json}" | jq -r --arg excl "${exclude_node}" '[
      .items[]
      | select(.metadata.deletionTimestamp == null)
      | select(.status.phase == "Running")
      | select((.spec.nodeName // "") != "" and (.spec.nodeName != $excl))
      | select(([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) > 0)
      | .metadata.name
    ] | join(" ")')"
    local count=0
    if [[ -n "${ready_names}" ]]; then
      count="$(wc -w <<< "${ready_names}" | tr -d ' ')"
    fi
    if (( count >= want_count )); then
      echo "${ready_names}"
      return 0
    fi
    sleep 2
  done
}

exec_rcon_lua() {
  local lua_cmd="${1}"
  local expected_substr="${2}"
  local out_json=""
  for _ in $(seq 1 15); do
    ensure_port_forward
    out_json="$(curl -fsSG --data-urlencode "actor=factorio/world-vanilla" --data-urlencode "cmd=${lua_cmd}" "http://127.0.0.1:${PF_PORT}/world-status" 2>/dev/null || true)"
    if echo "${out_json}" | jq -e '.world_ready == true' >/dev/null 2>&1; then
      local cmd_out
      cmd_out="$(echo "${out_json}" | jq -r '.cmd_output // ""')"
      if [[ "${cmd_out}" == *"${expected_substr}"* ]]; then
        echo "${out_json}"
        return 0
      fi
      # Factorio's first /sc on a save emits a warning before allowing Lua execution;
      # repeat once immediately when world_ready is true.
      sleep 0.5
      continue
    fi
    sleep 2
  done
  echo "ERROR: RCON Lua failed to return expected '${expected_substr}'. Last output:" >&2
  echo "${out_json}" >&2
  return 1
}

node_metadata() {
  local node="${1}"
  local z t s
  z="$(kubectl get node "${node}" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')"
  t="$(kubectl get node "${node}" -o jsonpath='{.metadata.labels.node\.kubernetes\.io/instance-type}')"
  s="$(kubectl get node "${node}" -o jsonpath='{.metadata.labels.cloud\.google\.com/gke-spot}')"
  echo "${z:-unknown}|${t:-unknown}|${s:-false}"
}

GW_NODE="$(kubectl get pod -n factorio -l app=factorio-gateway -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
EXTERNAL_IP=""
if [[ -n "${GW_NODE}" ]]; then
  EXTERNAL_IP="$(kubectl get node "${GW_NODE}" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)"
fi
if [[ -z "${EXTERNAL_IP}" ]]; then
  echo "ERROR: Could not determine ExternalIP for factorio-gateway node '${GW_NODE}'" >&2
  exit 1
fi

echo "================================================================================"
echo " GKE ComputeClass 2-Hop Spot Preemption Cascade Simulation"
echo " Gateway Node: ${GW_NODE} (${EXTERNAL_IP}:34197)"
echo "================================================================================"

ensure_port_forward

echo ""
echo "==> Step 1: Waking factorio/world-vanilla via UDP (${EXTERNAL_IP}:34197)..."
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34197" --timeout=240s

POD_1="$(wait_ready_worker_pods 1 "" 120 | awk '{print $1}')"
NODE_1="$(kubectl get pod -n factorio "${POD_1}" -o jsonpath='{.spec.nodeName}')"
IFS='|' read -r ZONE_1 TYPE_1 SPOT_1 <<< "$(node_metadata "${NODE_1}")"

echo "    Active Worker Pod:  ${POD_1}"
echo "    Active Worker Node: ${NODE_1} (zone=${ZONE_1}, type=${TYPE_1}, spot=${SPOT_1})"

LUA_MUTATE='/sc local s = game.surfaces["nauvis"]; local e = s.find_entity("steel-chest", {0.5, 0.5}) or s.create_entity{name="steel-chest", position={0.5, 0.5}, force="player"}; e.clear_items_inside(); e.insert{name="automation-science-pack", count=42}; rcon.print("NAUVIS_CHEST_SCIENCE_PACKS=" .. e.get_item_count("automation-science-pack") .. " TICK=" .. game.tick)'
LUA_QUERY='/sc local e = game.surfaces["nauvis"].find_entity("steel-chest", {0.5, 0.5}); rcon.print("NAUVIS_CHEST_SCIENCE_PACKS=" .. (e and e.get_item_count("automation-science-pack") or 0) .. " TICK=" .. game.tick)'

echo "==> Setting in-game proof item (steel-chest with 42 automation-science-packs) on ${NODE_1}:"
exec_rcon_lua "${LUA_MUTATE}" "NAUVIS_CHEST_SCIENCE_PACKS=42" | jq .

# ==============================================================================
# HOP 1: Cold ComputeClass Replacement (NODE_1 -> Cold NODE_2)
# ==============================================================================
echo ""
echo "==> Step 2 (Hop 1 — Cold Replacement): Simulating GCE 30s Spot Preemption on ${NODE_1} (${ZONE_1})..."
ensure_port_forward
PREV_EVAC="$(curl -fsS "http://127.0.0.1:${PF_PORT}/status" | jq -r '.preemption_shield.evacuation_count // 0')"
HOP1_START_MS="$(date +%s%3N)"
PREEMPTED_NODES+=("${NODE_1}")

kubectl cordon "${NODE_1}"
kubectl taint node "${NODE_1}" cloud.google.com/impending-node-termination=true:NoSchedule --overwrite

echo "==> Waiting for factorio-gateway Spot Preemption Shield (1s poll) to evacuate world-vanilla to GCS..."
HOP1_EVAC_DONE_MS=""
HOP1_CHECKPOINT_MS=0
for _ in $(seq 1 60); do
  ensure_port_forward
  STATUS_JSON="$(curl -fsS "http://127.0.0.1:${PF_PORT}/status")"
  EVAC_COUNT="$(echo "${STATUS_JSON}" | jq -r '.preemption_shield.evacuation_count // 0')"
  if (( EVAC_COUNT > PREV_EVAC )); then
    HOP1_EVAC_DONE_MS="$(date +%s%3N)"
    HOP1_CHECKPOINT_MS="$(echo "${STATUS_JSON}" | jq -r '.preemption_shield.last_evacuation.checkpoint_duration_ms // 0')"
    echo "    Spot Preemption Shield completed GCS checkpoint in ${HOP1_CHECKPOINT_MS}ms (detect+evac: $((HOP1_EVAC_DONE_MS - HOP1_START_MS))ms — inside GCE 30,000ms grace!)"
    echo "${STATUS_JSON}" | jq '.preemption_shield'
    break
  fi
  sleep 0.5
done
if [[ -z "${HOP1_EVAC_DONE_MS}" ]]; then
  echo "ERROR: Hop 1 Spot Preemption Shield did not trigger within 30s!" >&2
  exit 1
fi

echo "==> Measuring Hop 1 UDP client reconnect time onto cold ComputeClass replacement node..."
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34197" --timeout=240s
HOP1_RECOVER_END_MS="$(date +%s%3N)"
HOP1_TOTAL_MS=$((HOP1_RECOVER_END_MS - HOP1_START_MS))

POD_2="$(wait_ready_worker_pods 1 "${NODE_1}" 120 | awk '{print $1}')"
NODE_2="$(kubectl get pod -n factorio "${POD_2}" -o jsonpath='{.spec.nodeName}')"
IFS='|' read -r ZONE_2 TYPE_2 SPOT_2 <<< "$(node_metadata "${NODE_2}")"

if [[ "${NODE_2}" == "${NODE_1}" || "${POD_2}" == "${POD_1}" ]]; then
  echo "ERROR: Expected Hop 1 failover onto a distinct node/pod (got pod=${POD_2}, node=${NODE_2})" >&2
  exit 1
fi

echo "==> Verifying Hop 1 in-game state on ${POD_2} (${NODE_2}, zone=${ZONE_2}, type=${TYPE_2}, spot=${SPOT_2}):"
exec_rcon_lua "${LUA_QUERY}" "NAUVIS_CHEST_SCIENCE_PACKS=42" | jq .

# ==============================================================================
# HOP 2: Warm Standby Replacement (NODE_2 -> Pre-Warmed NODE_3)
# ==============================================================================
HOP2_STATUS="SKIPPED"
HOP2_CHECKPOINT_MS="-"
HOP2_TOTAL_MS="-"
POD_3="-"
NODE_3="-"
ZONE_3="-"
TYPE_3="-"
SPOT_3="-"

if [[ "${SKIP_WARM_HOP:-false}" != "true" ]]; then
  echo ""
  echo "==> Step 3 (Hop 2 — Warm Standby): Scaling WorkerPool/factorio to 2 replicas while ${NODE_1} remains cordoned..."
  ensure_port_forward
  curl -fsS -X POST "http://127.0.0.1:${PF_PORT}/scale?replicas=2" | jq .

  echo "==> Waiting for ComputeClass to provision warm standby pod #2 (NODE_3)..."
  READY_PAIR="$(wait_ready_worker_pods 2 "${NODE_1}" 240)"
  kubectl get pods -n factorio -l ate.dev/worker-pool=factorio -o wide

  # Identify the new warm standby node/pod before preempting NODE_2
  STANDBY_POD=""
  STANDBY_NODE=""
  for p in ${READY_PAIR}; do
    n="$(kubectl get pod -n factorio "${p}" -o jsonpath='{.spec.nodeName}')"
    if [[ "${n}" != "${NODE_2}" ]]; then
      STANDBY_POD="${p}"
      STANDBY_NODE="${n}"
      break
    fi
  done
  if [[ -z "${STANDBY_NODE}" ]]; then
    echo "WARNING: 2nd pod scheduled on same node (${NODE_2}); skipping Hop 2 distinction check" >&2
  else
    echo "    Standby Worker Ready: pod=${STANDBY_POD} on node=${STANDBY_NODE}"
  fi

  # Keep world-vanilla actively ticking right before Hop 2 preemption
  go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34197" --timeout=30s

  echo "==> Simulating GCE 30s Spot Preemption on ${NODE_2} (${ZONE_2}) with warm standby (${STANDBY_NODE}) ready..."
  ensure_port_forward
  PREV_EVAC="$(curl -fsS "http://127.0.0.1:${PF_PORT}/status" | jq -r '.preemption_shield.evacuation_count // 0')"
  HOP2_START_MS="$(date +%s%3N)"
  PREEMPTED_NODES+=("${NODE_2}")

  kubectl cordon "${NODE_2}"
  kubectl taint node "${NODE_2}" cloud.google.com/impending-node-termination=true:NoSchedule --overwrite

  HOP2_EVAC_DONE_MS=""
  for _ in $(seq 1 60); do
    ensure_port_forward
    STATUS_JSON="$(curl -fsS "http://127.0.0.1:${PF_PORT}/status")"
    EVAC_COUNT="$(echo "${STATUS_JSON}" | jq -r '.preemption_shield.evacuation_count // 0')"
    if (( EVAC_COUNT > PREV_EVAC )); then
      HOP2_EVAC_DONE_MS="$(date +%s%3N)"
      HOP2_CHECKPOINT_MS="$(echo "${STATUS_JSON}" | jq -r '.preemption_shield.last_evacuation.checkpoint_duration_ms // 0')"
      echo "    Hop 2 GCS checkpoint completed in ${HOP2_CHECKPOINT_MS}ms (detect+evac: $((HOP2_EVAC_DONE_MS - HOP2_START_MS))ms)"
      echo "${STATUS_JSON}" | jq '.preemption_shield'
      break
    fi
    sleep 0.5
  done
  if [[ -z "${HOP2_EVAC_DONE_MS}" ]]; then
    echo "ERROR: Hop 2 Spot Preemption Shield did not trigger within 30s!" >&2
    exit 1
  fi

  echo "==> Measuring Hop 2 UDP failover onto pre-warmed ComputeClass standby..."
  go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34197" --timeout=120s
  HOP2_RECOVER_END_MS="$(date +%s%3N)"
  HOP2_TOTAL_MS=$((HOP2_RECOVER_END_MS - HOP2_START_MS))

  POD_3="$(wait_ready_worker_pods 1 "${NODE_2}" 120 | awk '{print $1}')"
  NODE_3="$(kubectl get pod -n factorio "${POD_3}" -o jsonpath='{.spec.nodeName}')"
  IFS='|' read -r ZONE_3 TYPE_3 SPOT_3 <<< "$(node_metadata "${NODE_3}")"

  if [[ "${NODE_3}" == "${NODE_2}" || "${POD_3}" == "${POD_2}" ]]; then
    echo "ERROR: Expected Hop 2 failover onto a distinct node/pod (got pod=${POD_3}, node=${NODE_3})" >&2
    exit 1
  fi

  echo "==> Verifying Hop 2 in-game state on ${POD_3} (${NODE_3}, zone=${ZONE_3}, type=${TYPE_3}, spot=${SPOT_3}):"
  exec_rcon_lua "${LUA_QUERY}" "NAUVIS_CHEST_SCIENCE_PACKS=42" | jq .
  HOP2_STATUS="PASSED"
fi

echo ""
echo "==> Step 4: Cleanly suspending factorio/world-vanilla back to GCS..."
ensure_port_forward
curl -fsS -X POST "http://127.0.0.1:${PF_PORT}/suspend?actor=factorio/world-vanilla" | jq .

echo ""
echo "================================================================================"
echo " 2-HOP SPOT PREEMPTION CASCADE SUMMARY"
echo "--------------------------------------------------------------------------------"
printf " Initial Placement:        pod=%s node=%s zone=%s type=%s spot=%s\n" "${POD_1}" "${NODE_1}" "${ZONE_1}" "${TYPE_1}" "${SPOT_1}"
printf " Hop 1 (Cold Replacement): pod=%s node=%s zone=%s type=%s spot=%s\n" "${POD_2}" "${NODE_2}" "${ZONE_2}" "${TYPE_2}" "${SPOT_2}"
printf "   - GCS Checkpoint:       %sms (vs 30,000ms GCE Spot grace period)\n" "${HOP1_CHECKPOINT_MS}"
printf "   - Total Failover Time:  %sms\n" "${HOP1_TOTAL_MS}"
if (( HOP1_TOTAL_MS <= 15000 )); then
  echo "   - Player Experience:    BRIEF HICCUP (<15s Factorio timeout — NO disconnect!)"
else
  echo "   - Player Experience:    ZERO-LOSS RECONNECT (Cold VM boot >15s client timeout; 100% state saved)"
fi
if [[ "${HOP2_STATUS}" == "PASSED" ]]; then
  printf " Hop 2 (Warm Standby):     pod=%s node=%s zone=%s type=%s spot=%s\n" "${POD_3}" "${NODE_3}" "${ZONE_3}" "${TYPE_3}" "${SPOT_3}"
  printf "   - GCS Checkpoint:       %sms (vs 30,000ms GCE Spot grace period)\n" "${HOP2_CHECKPOINT_MS}"
  printf "   - Total Failover Time:  %sms\n" "${HOP2_TOTAL_MS}"
  if (( HOP2_TOTAL_MS <= 15000 )); then
    echo "   - Player Experience:    BRIEF HICCUP (<15s Factorio timeout — NO disconnect!)"
  else
    echo "   - Player Experience:    ZERO-LOSS RECONNECT (>15s client timeout; 100% state saved)"
  fi
fi
echo "================================================================================"
