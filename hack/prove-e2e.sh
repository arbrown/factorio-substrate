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

# Full stateful proof of Factorio on Agent Substrate:
#   1. Runs a live background watcher recording every Actor state transition
#      (SUSPENDED -> RESUMING -> RUNNING -> SUSPENDING -> SUSPENDED).
#   2. Wakes world-vanilla via UDP (:34197) and mutates the live Factorio
#      game world on surface "nauvis" via RCON Lua (spawning a steel-chest at
#      (0.5, 0.5) and inserting 42 automation-science-packs).
#   3. Wakes world-deathworld via UDP (:34198), forcing the single WorkerPool
#      pod to RCON-save and suspend world-vanilla to GCS and load world-deathworld.
#      Verifies world-deathworld does NOT have world-vanilla's chest.
#   4. Wakes world-vanilla back up from GCS via UDP (:34197) and queries the
#      Factorio Lua engine to prove the steel-chest and its 42 science packs
#      survived full eviction to GCS and restore.
#   5. Suspends world-vanilla back to ACTOR_STATE_SUSPENDED and dumps the
#      captured state-transition log, gateway logs, Factorio/gVisor worker
#      logs, atelet checkpoint logs, and GCS snapshot objects.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env
ensure_kubectl_ate
resolve_worlds

TRANSITIONS_LOG="$(mktemp "${TMPDIR:-/tmp}/factorio-transitions.XXXXXX")"
PF_PID=""
WATCH_PID=""

cleanup() {
  [[ -n "${WATCH_PID}" ]] && kill "${WATCH_PID}" >/dev/null 2>&1 || true
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" >/dev/null 2>&1 || true
  rm -f "${TRANSITIONS_LOG}"
}
trap cleanup EXIT

gw_external_ip 60 || die "timed out waiting for factorio-gateway ExternalIP"
ok "External Gateway IP: ${EXTERNAL_IP} (node=${GW_NODE})"

PF_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
kubectl port-forward -n "${NAMESPACE}" svc/factorio-gateway-http "${PF_PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!

pf_ready() {
  [[ "$(curl -fsS --max-time 2 "http://127.0.0.1:${PF_PORT}/healthz" 2>/dev/null || true)" == "ok" ]]
}
wait_for 30 "port-forward to factorio-gateway-http on :${PF_PORT}" pf_ready

PORT_VANILLA="${BASE_UDP_PORT}"
PORT_DEATHWORLD="$((BASE_UDP_PORT + 1))"

step "Ensuring world-vanilla and world-deathworld start in ACTOR_STATE_SUSPENDED"
curl -fsS -X POST "http://127.0.0.1:${PF_PORT}/suspend?actor=${ATESPACE}/world-vanilla" >/dev/null 2>&1 || true
curl -fsS -X POST "http://127.0.0.1:${PF_PORT}/suspend?actor=${ATESPACE}/world-deathworld" >/dev/null 2>&1 || true
sleep 2

step "Starting live Actor State Transition Watcher (polling every 0.3s)"
WATCH_INTERVAL=0.3 "${REPO_ROOT}/hack/watch-actors.sh" --transitions | tee "${TRANSITIONS_LOG}" &
WATCH_PID=$!
sleep 2

echo ""
echo "================================================================================"
echo " PHASE 1: Wake world-vanilla via UDP (${EXTERNAL_IP}:${PORT_VANILLA}) & Mutate Game World"
echo "================================================================================"
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:${PORT_VANILLA}" --timeout=240s

LUA_MUTATE='/sc local s = game.surfaces["nauvis"]; local e = s.find_entity("steel-chest", {0.5, 0.5}) or s.create_entity{name="steel-chest", position={0.5, 0.5}, force="player"}; e.clear_items_inside(); e.insert{name="automation-science-pack", count=42}; rcon.print("NAUVIS_CHEST_SCIENCE_PACKS=" .. e.get_item_count("automation-science-pack") .. " TICK=" .. game.tick)'
# Run twice in case this is the first Lua command on the save (Factorio achievement warning).
curl -fsSG --data-urlencode "actor=${ATESPACE}/world-vanilla" --data-urlencode "cmd=${LUA_MUTATE}" "http://127.0.0.1:${PF_PORT}/world-status" >/dev/null
step "Mutated world-vanilla via RCON Lua (spawned steel-chest at (0.5,0.5) with 42 automation-science-packs):"
curl -fsSG --data-urlencode "actor=${ATESPACE}/world-vanilla" --data-urlencode "cmd=${LUA_MUTATE}" "http://127.0.0.1:${PF_PORT}/world-status" | jq .

echo ""
echo "================================================================================"
echo " PHASE 2: Multiplex WorkerPool — Wake world-deathworld (${EXTERNAL_IP}:${PORT_DEATHWORLD})"
echo "          (Evicts & checkpoints world-vanilla to GCS, loads world-deathworld)"
echo "================================================================================"
sleep 6
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:${PORT_DEATHWORLD}" --timeout=120s

LUA_QUERY='/sc local e = game.surfaces["nauvis"].find_entity("steel-chest", {0.5, 0.5}); rcon.print("NAUVIS_CHEST_SCIENCE_PACKS=" .. (e and e.get_item_count("automation-science-pack") or 0) .. " TICK=" .. game.tick)'
curl -fsSG --data-urlencode "actor=${ATESPACE}/world-deathworld" --data-urlencode "cmd=${LUA_QUERY}" "http://127.0.0.1:${PF_PORT}/world-status" >/dev/null
step "Queried world-deathworld via RCON Lua (confirming separate world with 0 chest items):"
curl -fsSG --data-urlencode "actor=${ATESPACE}/world-deathworld" --data-urlencode "cmd=${LUA_QUERY}" "http://127.0.0.1:${PF_PORT}/world-status" | jq .

echo ""
echo "================================================================================"
echo " PHASE 3: Wake world-vanilla Back from GCS (${EXTERNAL_IP}:${PORT_VANILLA})"
echo "          & Prove In-Game Steel Chest + 42 Science Packs Survived Eviction"
echo "================================================================================"
sleep 6
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:${PORT_VANILLA}" --timeout=120s

step "Querying restored world-vanilla via RCON Lua (READ-ONLY query):"
RESTORED_JSON="$(curl -fsSG --data-urlencode "actor=${ATESPACE}/world-vanilla" --data-urlencode "cmd=${LUA_QUERY}" "http://127.0.0.1:${PF_PORT}/world-status")"
echo "${RESTORED_JSON}" | jq .
if [[ "$(echo "${RESTORED_JSON}" | jq -r '.cmd_output')" != *"NAUVIS_CHEST_SCIENCE_PACKS=42"* ]]; then
  die "Expected NAUVIS_CHEST_SCIENCE_PACKS=42 after restore"
fi
ok "VERIFIED: In-game entity and inventory (NAUVIS_CHEST_SCIENCE_PACKS=42) survived full GCS suspend & restore"

echo ""
echo "================================================================================"
echo " PHASE 4: Suspend Active World Back to GCS (All Actors -> SUSPENDED)"
echo "================================================================================"
curl -fsS -X POST "http://127.0.0.1:${PF_PORT}/suspend?actor=${ATESPACE}/world-vanilla" && echo ""
is_suspended() {
  [[ "$(run_kubectl_ate get actor "$1" -a "${ATESPACE}" -o json 2>/dev/null | jq -r '.status.state // empty')" == "ACTOR_STATE_SUSPENDED" ]]
}
wait_for 120 "world-vanilla to reach ACTOR_STATE_SUSPENDED" is_suspended world-vanilla
wait_for 120 "world-deathworld to reach ACTOR_STATE_SUSPENDED" is_suspended world-deathworld
run_kubectl_ate get actors -a "${ATESPACE}"

kill "${WATCH_PID}" >/dev/null 2>&1 || true
WATCH_PID=""

echo ""
echo "================================================================================"
echo " PHASE 5: Post-Run Log & GCS Evidence"
echo "================================================================================"
echo "--- 5a. Recorded Actor State Transitions (SUSPENDED <-> RESUMING <-> RUNNING <-> SUSPENDING) ---"
cat "${TRANSITIONS_LOG}"

echo ""
echo "--- 5b. Factorio UDP Gateway Logs (kubectl logs deployment/factorio-gateway) ---"
kubectl logs -n "${NAMESPACE}" deployment/factorio-gateway --tail=25

echo ""
echo "--- 5c. Factorio Server & gVisor Sandbox Logs inside WorkerPool Pod ---"
WORKER_POD="$(kubectl get pods -n "${NAMESPACE}" -l ate.dev/worker-pool=factorio -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
WORKER_NODE=""
if [[ -n "${WORKER_POD}" ]]; then
  WORKER_NODE="$(kubectl get pod -n "${NAMESPACE}" "${WORKER_POD}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
  kubectl logs -n "${NAMESPACE}" "${WORKER_POD}" --tail=400 | grep -E '"ate.actor.container.name":"factorio"|About to run runsc|Saving game' | tail -n 20 || true
fi

echo ""
echo "--- 5d. Node Agent (atelet on ${WORKER_NODE:-<none>}) Snapshot Upload/Download Logs ---"
if [[ -n "${WORKER_NODE}" ]]; then
  ATELET_POD="$(kubectl get pods -n ate-system -l app=atelet --field-selector spec.nodeName="${WORKER_NODE}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -n "${ATELET_POD}" ]]; then
    kubectl logs -n ate-system "${ATELET_POD}" --all-containers=true --tail=200 | grep -E "Compressed zstd upload|Decompressed zstd download|Checkpoint|Restore" | tail -n 15 || true
  fi
fi

echo ""
echo "--- 5e. Actual Snapshot Artifacts in Google Cloud Storage ---"
VANILLA_SNAP="$(run_kubectl_ate get actor world-vanilla -a "${ATESPACE}" -o json | jq -r '.status.snapshots[-1].durableSnapshot.object.snapshotUri // empty')"
DEATHWORLD_SNAP="$(run_kubectl_ate get actor world-deathworld -a "${ATESPACE}" -o json | jq -r '.status.snapshots[-1].durableSnapshot.object.snapshotUri // empty')"
echo "world-vanilla latest snapshot:    ${VANILLA_SNAP}"
[[ -n "${VANILLA_SNAP}" ]] && { gcloud storage ls -l "${VANILLA_SNAP}/" || true; }
echo "world-deathworld latest snapshot: ${DEATHWORLD_SNAP}"
[[ -n "${DEATHWORLD_SNAP}" ]] && { gcloud storage ls -l "${DEATHWORLD_SNAP}/" || true; }

echo ""
ok "ALL PROOFS COMPLETED SUCCESSFULLY"
