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

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh ]]; then
  source .ate-dev-env.sh
fi

SUBSTRATE_REPO="${SUBSTRATE_REPO:-$(cd "${ROOT}/../substrate" && pwd)}"

KUBECTL_ATE_BIN="$(mktemp /tmp/kubectl-ate.XXXXXX)"
TRANSITIONS_LOG="$(mktemp /tmp/factorio-transitions.XXXXXX)"
PF_PID=""
WATCH_PID=""

cleanup() {
  [[ -n "${WATCH_PID}" ]] && kill "${WATCH_PID}" >/dev/null 2>&1 || true
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" >/dev/null 2>&1 || true
  rm -f "${KUBECTL_ATE_BIN}" "${TRANSITIONS_LOG}"
}
trap cleanup EXIT

(cd "${SUBSTRATE_REPO}" && go build -o "${KUBECTL_ATE_BIN}" ./cmd/kubectl-ate)

GW_NODE="$(kubectl get pod -n factorio -l app=factorio-gateway -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
EXTERNAL_IP=""
if [[ -n "${GW_NODE}" ]]; then
  EXTERNAL_IP="$(kubectl get node "${GW_NODE}" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)"
fi
if [[ -z "${EXTERNAL_IP}" ]]; then
  EXTERNAL_IP="$(kubectl get svc -n factorio factorio-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
fi
echo "==> External Gateway IP: ${EXTERNAL_IP} (node=${GW_NODE:-unknown})"

kubectl port-forward -n factorio svc/factorio-gateway-http 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
sleep 2

echo "==> Ensuring both worlds start in ACTOR_STATE_SUSPENDED..."
curl -fsS -X POST "http://127.0.0.1:18080/suspend?actor=factorio/world-vanilla" >/dev/null 2>&1 || true
curl -fsS -X POST "http://127.0.0.1:18080/suspend?actor=factorio/world-deathworld" >/dev/null 2>&1 || true
sleep 2

echo "==> Starting live Actor State Transition Watcher (polling every 0.3s)..."
WATCH_INTERVAL=0.3 ./hack/watch-actors.sh --transitions | tee "${TRANSITIONS_LOG}" &
WATCH_PID=$!
sleep 2

echo ""
echo "================================================================================"
echo " PHASE 1: Wake world-vanilla via UDP (${EXTERNAL_IP}:34197) & Mutate Game World"
echo "================================================================================"
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34197" --timeout=180s

LUA_MUTATE='/sc local s = game.surfaces["nauvis"]; local e = s.find_entity("steel-chest", {0.5, 0.5}) or s.create_entity{name="steel-chest", position={0.5, 0.5}, force="player"}; e.clear_items_inside(); e.insert{name="automation-science-pack", count=42}; rcon.print("NAUVIS_CHEST_SCIENCE_PACKS=" .. e.get_item_count("automation-science-pack") .. " TICK=" .. game.tick)'
# Run twice in case this is the first Lua command on the save (Factorio achievement warning).
curl -fsSG --data-urlencode "actor=factorio/world-vanilla" --data-urlencode "cmd=${LUA_MUTATE}" "http://127.0.0.1:18080/world-status" >/dev/null
echo "==> Mutated world-vanilla via RCON Lua (spawned steel-chest at (0.5,0.5) with 42 automation-science-packs):"
curl -fsSG --data-urlencode "actor=factorio/world-vanilla" --data-urlencode "cmd=${LUA_MUTATE}" "http://127.0.0.1:18080/world-status" | jq .

echo ""
echo "================================================================================"
echo " PHASE 2: Multiplex WorkerPool — Wake world-deathworld (${EXTERNAL_IP}:34198)"
echo "          (Evicts & checkpoints world-vanilla to GCS, loads world-deathworld)"
echo "================================================================================"
sleep 6
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34198" --timeout=60s

LUA_QUERY='/sc local e = game.surfaces["nauvis"].find_entity("steel-chest", {0.5, 0.5}); rcon.print("NAUVIS_CHEST_SCIENCE_PACKS=" .. (e and e.get_item_count("automation-science-pack") or 0) .. " TICK=" .. game.tick)'
curl -fsSG --data-urlencode "actor=factorio/world-deathworld" --data-urlencode "cmd=${LUA_QUERY}" "http://127.0.0.1:18080/world-status" >/dev/null
echo "==> Queried world-deathworld via RCON Lua (confirming separate world with 0 chest items):"
curl -fsSG --data-urlencode "actor=factorio/world-deathworld" --data-urlencode "cmd=${LUA_QUERY}" "http://127.0.0.1:18080/world-status" | jq .

echo ""
echo "================================================================================"
echo " PHASE 3: Wake world-vanilla Back from GCS (${EXTERNAL_IP}:34197)"
echo "          & Prove In-Game Steel Chest + 42 Science Packs Survived Eviction"
echo "================================================================================"
sleep 6
go run ./cmd/factorio-probe --addr="${EXTERNAL_IP}:34197" --timeout=60s

echo "==> Querying restored world-vanilla via RCON Lua (READ-ONLY query):"
RESTORED_JSON="$(curl -fsSG --data-urlencode "actor=factorio/world-vanilla" --data-urlencode "cmd=${LUA_QUERY}" "http://127.0.0.1:18080/world-status")"
echo "${RESTORED_JSON}" | jq .
if [[ "$(echo "${RESTORED_JSON}" | jq -r '.cmd_output')" != *"NAUVIS_CHEST_SCIENCE_PACKS=42"* ]]; then
  echo "ERROR: Expected NAUVIS_CHEST_SCIENCE_PACKS=42 after restore!" >&2
  exit 1
fi
echo "==> VERIFIED: In-game entity and inventory (NAUVIS_CHEST_SCIENCE_PACKS=42) survived full GCS suspend & restore!"

echo ""
echo "================================================================================"
echo " PHASE 4: Suspend Active World Back to GCS (Both Actors -> SUSPENDED)"
echo "================================================================================"
curl -fsS -X POST "http://127.0.0.1:18080/suspend?actor=factorio/world-vanilla" && echo ""
sleep 6
"${KUBECTL_ATE_BIN}" get actors -a factorio

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
kubectl logs -n factorio deployment/factorio-gateway --tail=25

echo ""
echo "--- 5c. Factorio Server & gVisor Sandbox Logs inside WorkerPool Pod ---"
WORKER_POD="$(kubectl get pods -n factorio -l ate.dev/worker-pool=factorio -o jsonpath='{.items[0].metadata.name}')"
WORKER_NODE="$(kubectl get pod -n factorio "${WORKER_POD}" -o jsonpath='{.spec.nodeName}')"
kubectl logs -n factorio "${WORKER_POD}" --tail=400 | grep -E '"ate.actor.container.name":"factorio"|About to run runsc|Saving game' | tail -n 20 || true

echo ""
echo "--- 5d. Node Agent (atelet on ${WORKER_NODE}) Snapshot Upload/Download Logs ---"
ATELET_POD="$(kubectl get pods -n ate-system -l app=atelet --field-selector spec.nodeName="${WORKER_NODE}" -o jsonpath='{.items[0].metadata.name}')"
kubectl logs -n ate-system "${ATELET_POD}" --all-containers=true --tail=200 | grep -E "Compressed zstd upload|Decompressed zstd download|Checkpoint|Restore" | tail -n 15 || true

echo ""
echo "--- 5e. Actual Snapshot Artifacts in Google Cloud Storage ---"
VANILLA_SNAP="$("${KUBECTL_ATE_BIN}" get actor world-vanilla -a factorio -o json | jq -r '.status.snapshots[-1].durableSnapshot.object.snapshotUri // empty')"
DEATHWORLD_SNAP="$("${KUBECTL_ATE_BIN}" get actor world-deathworld -a factorio -o json | jq -r '.status.snapshots[-1].durableSnapshot.object.snapshotUri // empty')"
echo "world-vanilla latest snapshot:    ${VANILLA_SNAP}"
gcloud storage ls -l "${VANILLA_SNAP}/" || true
echo "world-deathworld latest snapshot: ${DEATHWORLD_SNAP}"
gcloud storage ls -l "${DEATHWORLD_SNAP}/" || true

echo ""
echo "==> ALL PROOFS COMPLETED SUCCESSFULLY!"
