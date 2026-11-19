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

# Offline tests for hack/ scripts and manifests/*.yaml (no GCP or cluster needed).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

echo "==> Checking shell syntax across hack/*.sh"
bash -n hack/*.sh

echo "==> Validating all manifests/*.yaml files as standalone YAML"
python3 - <<'PY'
import glob, yaml
files = sorted(glob.glob("manifests/*.yaml"))
assert len(files) == 8, f"expected 8 manifest YAML files, got {files}"
for f in files:
    docs = list(yaml.safe_load_all(open(f)))
    assert docs and all(d is not None for d in docs), f"empty/invalid doc in {f}"
    print(f"    OK {f} ({len(docs)} doc(s))")
PY

echo "==> Testing load_env and YAML renderers against .env.example"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/factorio-script-test.XXXXXX")"
trap 'rm -rf "${TMP_DIR}"' EXIT

sed 's/^PROJECT_ID=$/PROJECT_ID=test-proj-123/' .env.example > "${TMP_DIR}/.env"
export FACTORIO_ENV_FILE="${TMP_DIR}/.env"
export FACTORIO_STATE_DIR="${TMP_DIR}/.state"

bash -euo pipefail <<'BASH'
source hack/lib.sh
load_env
[[ "${PROJECT_ID}" == "test-proj-123" ]]
[[ "${BUCKET_NAME}" == "test-proj-123-factorio-ate" ]]
[[ "${AR_REPO}" == "factorio-ate" ]]
[[ "${KO_DOCKER_REPO}" == "us-central1-docker.pkg.dev/test-proj-123/factorio-ate" ]]
[[ "${SUBSTRATE_IMAGE_REPO}" == "ghcr.io/agent-substrate/substrate" ]]
[[ "${GKE_CLUSTER_VERSION}" == "1.37" ]]
[[ "${ATE_API_LOG_LEVEL}" == "error" ]]
[[ "${GKE_LOGGING}" == "SYSTEM" ]]
[[ "${GKE_MONITORING}" == "SYSTEM" ]]
[[ "${GKE_MANAGED_PROMETHEUS}" == "false" ]]

# Verify CLI overrides beat .env values
IDLE_TIMEOUT=45s WORLDS_LIST="world-a world-b" WORKER_ZONES="us-east1-b,us-east1-c" ATE_API_LOG_LEVEL=debug bash -euo pipefail <<'INNER'
source hack/lib.sh
load_env
[[ "${IDLE_TIMEOUT}" == "45s" ]]
[[ "${WORLDS_LIST}" == "world-a world-b" ]]
[[ "${ATE_API_LOG_LEVEL}" == "debug" ]]

render_computeclass_yaml | python3 -c '
import sys, yaml
d = yaml.safe_load(sys.stdin)
zones = d["spec"]["priorityDefaults"]["location"]["zones"]
assert zones == ["us-east1-b", "us-east1-c"], zones
'

WORKER_IMAGE="ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.0@sha256:deadbeef" \
  render_workerpool_yaml | python3 -c '
import sys, yaml
d = yaml.safe_load(sys.stdin)
assert d["spec"]["replicas"] == 1
assert d["spec"]["workerImage"].endswith("@sha256:deadbeef")
'

render_gateway_yaml | python3 -c '
import sys, yaml
docs = list(yaml.safe_load_all(sys.stdin))
c = docs[0]["spec"]["template"]["spec"]["containers"][0]
assert "--idle-timeout=45s" in c["args"], c["args"]
assert "--routes=34197=factorio/world-a,34198=factorio/world-b" in c["args"], c["args"]
udp = [p["containerPort"] for p in c["ports"] if p["protocol"] == "UDP"]
tcp = [p["containerPort"] for p in c["ports"] if p["protocol"] == "TCP"]
assert udp == [34197, 34198], udp
assert tcp == [8080], tcp
'

FACTORIO_IMAGE="index.docker.io/factoriotools/factorio@sha256:cafe" \
  render_actor_template_yaml | python3 -c '
import sys, yaml
d = yaml.safe_load(sys.stdin)
f = d["containers"][1]
assert f["image"] == "index.docker.io/factoriotools/factorio@sha256:cafe"
script = f["command"][2]
for var in ("${TARGET_ACTOR}", "${SAVED_ACTOR}", "${FACTORIO_PID}"):
    assert var in script, f"missing runtime shell var {var}"
'
INNER
BASH

echo "==> Offline shell and renderer tests PASSED"
