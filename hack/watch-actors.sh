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

# Live watcher for Factorio Actors on Agent Substrate.
# Modes:
#   ./hack/watch-actors.sh                  # Full live-updating terminal dashboard
#   ./hack/watch-actors.sh --transitions    # Timestamped log of every Actor state change

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh ]]; then
  source .ate-dev-env.sh
fi

SUBSTRATE_REPO="${SUBSTRATE_REPO:-$(cd "${ROOT}/../substrate" && pwd)}"
INTERVAL="${WATCH_INTERVAL:-1}"
MODE="${1:-dashboard}"

KUBECTL_ATE_BIN="$(mktemp /tmp/kubectl-ate.XXXXXX)"
trap 'rm -f "${KUBECTL_ATE_BIN}"' EXIT
(cd "${SUBSTRATE_REPO}" && go build -o "${KUBECTL_ATE_BIN}" ./cmd/kubectl-ate)

if [[ "${MODE}" == "--transitions" ]]; then
  echo "[$(date +%H:%M:%S)] Watching Factorio Actors in atespace 'factorio' for state transitions..."
  declare -A prev_line
  while true; do
    while IFS=$'\t' read -r name state pod ver; do
      [[ -z "${name}" ]] && continue
      summary="${state} (pod=${pod}, version=${ver})"
      if [[ "${prev_line[${name}]:-}" != "${summary}" ]]; then
        if [[ -n "${prev_line[${name}]:-}" ]]; then
          echo "[$(date +%H:%M:%S)] TRANSITION ${name}: ${prev_line[${name}]}  -->  ${summary}"
        else
          echo "[$(date +%H:%M:%S)] INITIAL    ${name}: ${summary}"
        fi
        prev_line["${name}"]="${summary}"
      fi
    done < <("${KUBECTL_ATE_BIN}" get actors -a factorio -o json 2>/dev/null | jq -r '.actors[]? | [.metadata.name, .status.state, ((.status.workerAssignment.workerPodNamespace // "") + "/" + (.status.workerAssignment.workerPodName // "") | if . == "/" then "<none>" else . end), .metadata.version] | @tsv')
    sleep "${INTERVAL}"
  done
else
  while true; do
    clear
    echo "================================================================================"
    echo " Factorio on Agent Substrate — Live Actor & Gateway Watch ($(date +%H:%M:%S))"
    echo "================================================================================"
    echo ""
    echo "--- Substrate Actors (kubectl-ate get actors -a factorio) ---"
    "${KUBECTL_ATE_BIN}" get actors -a factorio || true
    echo ""
    echo "--- UDP Gateway Status (:8080/status) ---"
    kubectl get --raw /api/v1/namespaces/factorio/services/factorio-gateway-http:status-http/proxy/status 2>/dev/null | jq . || true
    sleep "${INTERVAL}"
  done
fi
