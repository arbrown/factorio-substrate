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

# Shared helpers for Factorio on Agent Substrate scripts. Source this file; do not execute it.
case $- in *i*)
  echo "hack/lib.sh is for automation scripts only (it enables set -euo pipefail)." >&2
  return 1 ;;
esac
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${FACTORIO_STATE_DIR:-${REPO_ROOT}/.state}"
: "${FACTORIO_USER_PATH:=${PATH}}"; export FACTORIO_USER_PATH
GOPATH_BIN="$(go env GOPATH 2>/dev/null || true)/bin"
export PATH="${REPO_ROOT}/bin:${GOPATH_BIN}:${PATH}"

# Avoid interactive gcloud prompts, inherited quota-project surprises, parent go.work
# interference with Substrate checkouts, and Substrate reading a separate .ate-dev-env.sh.
export CLOUDSDK_CORE_DISABLE_PROMPTS=1
export CLOUDSDK_BILLING_QUOTA_PROJECT=""
export NO_DEV_ENV=1
export GOWORK=off

if [[ -t 1 ]]; then
  C_B=$'\e[1m'; C_G=$'\e[32m'; C_Y=$'\e[33m'; C_R=$'\e[31m'; C_0=$'\e[0m'
else
  C_B=""; C_G=""; C_Y=""; C_R=""; C_0=""
fi
step() { echo "${C_B}==> $*${C_0}"; }
ok()   { echo "${C_G}  ✓ $*${C_0}"; }
warn() { echo "${C_Y}  ! $*${C_0}" >&2; }
die()  { echo "${C_R}  ✗ $*${C_0}" >&2; exit 1; }

_OVERRIDE_VARS=(
  PROJECT_ID REGION CLUSTER_LOCATION CLUSTER_NAME WORKER_ZONES
  GKE_CHANNEL GKE_CLUSTER_VERSION CLUSTER_INIT_MACHINE_TYPE
  CONTROL_POOL_NAME CONTROL_POOL_MACHINE_TYPE CLUSTER_NODE_TAGS FIREWALL_RULE_NAME
  BUCKET_NAME SNAPSHOT_LOCATION SNAPSHOT_TRANSFER_GSA_NAME
  AR_REPO KO_DOCKER_REPO KO_DEFAULTPLATFORMS
  SUBSTRATE_REF SUBSTRATE_REPO SUBSTRATE_IMAGE_REPO SUBSTRATE_IMAGE_TAG
  ATE_CREDENTIAL_PROVIDER ATE_INSTALL_SANDBOX_CLASSES ATE_INSTALL_E2E_TUNING
  ATE_INGRESS_ALLOWED_SPIFFE_IDS ATE_API_AUTHZ_BOOTSTRAP_OWNERS ATE_ROLLOUT_TIMEOUT
  ATE_API_LOG_LEVEL GKE_LOGGING GKE_MONITORING GKE_MANAGED_PROMETHEUS
  NAMESPACE ATESPACE IDLE_TIMEOUT WORKERPOOL_IDLE_TIMEOUT MAX_ACTIVE_WORLDS
  PREEMPT_IDLE_GRACE BASE_UDP_PORT WORLDS_LIST FACTORIO_TAG FACTORIO_IMAGE
  RESET_WORLDS RECREATE_TEMPLATE PRUNE_EXTRA_WORLDS
  YES ALL DELETE_ORPHAN_DISKS VERIFY_IDLE_SUSPEND IDLE_WAIT_SECS SKIP_WARM_HOP SIMULATE_ZONE_STOCKOUT WATCH_INTERVAL
)

# load_env reads `.env` (or FACTORIO_ENV_FILE), preserving explicit caller/CLI overrides.
load_env() {
  local env_file="${FACTORIO_ENV_FILE:-${REPO_ROOT}/.env}"
  [[ -f "${env_file}" ]] || die "missing ${env_file} — run: cp .env.example .env and set PROJECT_ID"

  declare -A _caller_overrides=()
  local v
  for v in "${_OVERRIDE_VARS[@]}"; do
    if [[ -n "${!v+x}" ]]; then
      _caller_overrides["${v}"]="${!v}"
    fi
  done

  set -a
  # shellcheck disable=SC1090
  source "${env_file}"
  set +a

  for v in "${!_caller_overrides[@]}"; do
    printf -v "${v}" '%s' "${_caller_overrides[${v}]}"
    export "${v}"
  done

  [[ -n "${PROJECT_ID:-}" ]] || die "PROJECT_ID is empty in ${env_file}"
  export CLOUDSDK_CORE_PROJECT="${PROJECT_ID}"

  : "${REGION:=us-central1}"
  : "${CLUSTER_LOCATION:=${REGION}-a}"
  : "${CLUSTER_NAME:=factorio-ate}"
  : "${WORKER_ZONES:=${REGION}-a,${REGION}-b,${REGION}-c,${REGION}-f}"
  : "${GKE_CHANNEL:=regular}"
  : "${GKE_CLUSTER_VERSION:=1.37}"
  : "${CLUSTER_INIT_MACHINE_TYPE:=e2-standard-4}"
  : "${CONTROL_POOL_NAME:=control-pool}"
  : "${CONTROL_POOL_MACHINE_TYPE:=e2-medium}"
  : "${CLUSTER_NODE_TAGS:=factorio-gateway-node}"
  : "${FIREWALL_RULE_NAME:=${CLUSTER_NAME}-allow-udp-hostport}"

  : "${BUCKET_NAME:=${PROJECT_ID}-${CLUSTER_NAME}}"
  : "${SNAPSHOT_LOCATION:=gs://${BUCKET_NAME}/factorio/}"
  : "${SNAPSHOT_TRANSFER_GSA_NAME:=ate-snapshot-transfer}"
  AR_REPO="${AR_REPO-factorio-ate}"
  : "${KO_DOCKER_REPO:=${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO:-factorio-ate}}"
  : "${KO_DEFAULTPLATFORMS:=linux/amd64}"

  : "${SUBSTRATE_REF:=v1.0.0}"
  SUBSTRATE_IMAGE_REPO="${SUBSTRATE_IMAGE_REPO-}"
  SUBSTRATE_IMAGE_TAG="${SUBSTRATE_IMAGE_TAG-}"

  : "${ATE_CREDENTIAL_PROVIDER:={\"enabled\":false}}"
  : "${ATE_INSTALL_SANDBOX_CLASSES:=gvisor}"
  : "${ATE_INSTALL_E2E_TUNING:=true}"
  : "${NAMESPACE:=factorio}"
  : "${ATESPACE:=factorio}"
  : "${ATE_INGRESS_ALLOWED_SPIFFE_IDS:=spiffe://cluster.local/ns/ate-system/sa/atenet-client,spiffe://cluster.local/ns/${NAMESPACE}/sa/factorio-gateway}"
  : "${ATE_API_AUTHZ_BOOTSTRAP_OWNERS:=system:serviceaccount:ate-system:ate-client,spiffe://cluster.local/ns/${NAMESPACE}/sa/factorio-gateway}"
  : "${ATE_ROLLOUT_TIMEOUT:=5m}"
  : "${ATE_API_LOG_LEVEL:=error}"
  : "${GKE_LOGGING:=SYSTEM}"
  : "${GKE_MONITORING:=SYSTEM}"
  : "${GKE_MANAGED_PROMETHEUS:=false}"

  : "${IDLE_TIMEOUT:=10m}"
  : "${WORKERPOOL_IDLE_TIMEOUT:=2m}"
  : "${MAX_ACTIVE_WORLDS:=1}"
  : "${PREEMPT_IDLE_GRACE:=5s}"
  : "${BASE_UDP_PORT:=34197}"
  : "${FACTORIO_TAG:=stable}"

  export PROJECT_ID REGION CLUSTER_LOCATION CLUSTER_NAME WORKER_ZONES
  export GKE_CHANNEL GKE_CLUSTER_VERSION CLUSTER_INIT_MACHINE_TYPE
  export CONTROL_POOL_NAME CONTROL_POOL_MACHINE_TYPE CLUSTER_NODE_TAGS FIREWALL_RULE_NAME
  export BUCKET_NAME SNAPSHOT_LOCATION SNAPSHOT_TRANSFER_GSA_NAME
  export AR_REPO KO_DOCKER_REPO KO_DEFAULTPLATFORMS
  export SUBSTRATE_REF SUBSTRATE_IMAGE_REPO SUBSTRATE_IMAGE_TAG
  export ATE_CREDENTIAL_PROVIDER ATE_INSTALL_SANDBOX_CLASSES ATE_INSTALL_E2E_TUNING
  export ATE_INGRESS_ALLOWED_SPIFFE_IDS ATE_API_AUTHZ_BOOTSTRAP_OWNERS ATE_ROLLOUT_TIMEOUT
  export ATE_API_LOG_LEVEL GKE_LOGGING GKE_MONITORING GKE_MANAGED_PROMETHEUS
  export NAMESPACE ATESPACE IDLE_TIMEOUT WORKERPOOL_IDLE_TIMEOUT MAX_ACTIVE_WORLDS
  export PREEMPT_IDLE_GRACE BASE_UDP_PORT FACTORIO_TAG

  mkdir -p "${STATE_DIR}"
}

state_put() { mkdir -p "${STATE_DIR}"; printf '%s\n' "$2" > "${STATE_DIR}/$1"; }
state_get() { [[ -f "${STATE_DIR}/$1" ]] && cat "${STATE_DIR}/$1"; }

wait_for() {
  local timeout="$1" what="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    (( SECONDS >= deadline )) && die "timed out after ${timeout}s waiting for: ${what}"
    sleep 2
  done
}

# ensure_substrate_checkout sets SUBSTRATE_SRC to SUBSTRATE_REPO if configured,
# or clones/checks out SUBSTRATE_REF in .state/substrate.
ensure_substrate_checkout() {
  if [[ -n "${SUBSTRATE_REPO:-}" ]]; then
    [[ -d "${SUBSTRATE_REPO}" ]] || die "SUBSTRATE_REPO does not exist: ${SUBSTRATE_REPO}"
    SUBSTRATE_SRC="$(cd "${SUBSTRATE_REPO}" && pwd)"
    export SUBSTRATE_SRC
    return 0
  fi

  local src="${STATE_DIR}/substrate"
  if [[ ! -d "${src}/.git" ]]; then
    step "Cloning Agent Substrate (${SUBSTRATE_REF}) -> .state/substrate"
    git clone --quiet https://github.com/agent-substrate/substrate "${src}"
  fi
  local cur_ref desired_sha cur_sha
  desired_sha="$(git -C "${src}" rev-parse --verify "${SUBSTRATE_REF}^{commit}" 2>/dev/null || true)"
  cur_sha="$(git -C "${src}" rev-parse HEAD 2>/dev/null || true)"
  if [[ -z "${desired_sha}" || "${desired_sha}" != "${cur_sha}" ]]; then
    git -C "${src}" fetch --quiet --tags origin
    git -C "${src}" checkout --quiet "${SUBSTRATE_REF}"
  fi
  SUBSTRATE_SRC="${src}"
  export SUBSTRATE_SRC
}

# ensure_kubectl_ate builds bin/kubectl-ate from SUBSTRATE_SRC if missing or stale.
ensure_kubectl_ate() {
  ensure_substrate_checkout
  mkdir -p "${REPO_ROOT}/bin"
  local bin="${REPO_ROOT}/bin/kubectl-ate"
  local src_head ver stamp
  src_head="$(git -C "${SUBSTRATE_SRC}" rev-parse HEAD 2>/dev/null || echo unknown)"
  stamp="${SUBSTRATE_SRC}:${src_head}"
  if [[ ! -x "${bin}" ]] || [[ "$(state_get kubectl-ate.ref || true)" != "${stamp}" ]]; then
    step "Building bin/kubectl-ate from ${SUBSTRATE_SRC} (${SUBSTRATE_REF})"
    ver="$(git -C "${SUBSTRATE_SRC}" describe --tags --always --dirty 2>/dev/null || echo "${SUBSTRATE_REF}")"
    (cd "${SUBSTRATE_SRC}" && go build -ldflags "-X=go.ate.dev/substrate/internal/version.Version=${ver}" -o "${bin}" ./cmd/kubectl-ate)
    state_put kubectl-ate.ref "${stamp}"
  fi
  KUBECTL_ATE_BIN="${bin}"
  export KUBECTL_ATE_BIN
}

run_kubectl_ate() {
  [[ -n "${KUBECTL_ATE_BIN:-}" && -x "${KUBECTL_ATE_BIN}" ]] || ensure_kubectl_ate
  "${KUBECTL_ATE_BIN}" "$@"
}

ensure_ko() {
  if command -v ko >/dev/null 2>&1; then
    KO_BIN="$(command -v ko)"
  else
    ensure_substrate_checkout
    if [[ -x "${SUBSTRATE_SRC}/hack/run-tool.sh" ]]; then
      KO_BIN="$(cd "${SUBSTRATE_SRC}" && ./hack/run-tool.sh --print-bin-path ko)"
    fi
  fi
  [[ -n "${KO_BIN:-}" && -x "${KO_BIN}" ]] || die "ko not found on PATH — install with: go install github.com/google/ko@v0.19.1"
  export KO_BIN
}

run_ko() {
  ensure_ko
  "${KO_BIN}" "$@"
}

# resolve_worker_image resolves or builds the digest-pinned ateworker-gvisor image.
# Uses pre-built images when SUBSTRATE_IMAGE_REPO + SUBSTRATE_IMAGE_TAG are set;
# otherwise builds from SUBSTRATE_SRC with ko into KO_DOCKER_REPO.
resolve_worker_image() {
  local cache_key="${PROJECT_ID}|${KO_DOCKER_REPO}|${SUBSTRATE_IMAGE_REPO:-}|${SUBSTRATE_IMAGE_TAG:-}"
  local cache_file="${STATE_DIR}/worker-image.${PROJECT_ID}"
  if [[ -f "${cache_file}" ]]; then
    local cached_key cached_img
    IFS=$'\t' read -r cached_key cached_img < "${cache_file}" || true
    if [[ "${cached_key:-}" == "${cache_key}" && "${cached_img:-}" == *@sha256:* ]]; then
      WORKER_IMAGE="${cached_img}"
      export WORKER_IMAGE
      return 0
    fi
  fi

  local worker=""
  if [[ -n "${SUBSTRATE_IMAGE_REPO:-}" ]]; then
    local tag="${SUBSTRATE_IMAGE_TAG:-${SUBSTRATE_REF}}"
    step "Resolving published ateworker-gvisor digest (${SUBSTRATE_IMAGE_REPO}/ateworker-gvisor:${tag})..."
    local digest=""
    if command -v crane >/dev/null 2>&1; then
      digest="$(crane digest "${SUBSTRATE_IMAGE_REPO}/ateworker-gvisor:${tag}")"
    else
      digest="$(go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 digest "${SUBSTRATE_IMAGE_REPO}/ateworker-gvisor:${tag}")"
    fi
    worker="${SUBSTRATE_IMAGE_REPO}/ateworker-gvisor:${tag}@${digest}"
  else
    ensure_substrate_checkout
    ensure_ko
    step "Building ateworker-gvisor worker image from ${SUBSTRATE_SRC} via ko..."
    worker="$(cd "${SUBSTRATE_SRC}" && "${KO_BIN}" build ./cmd/ateworker-gvisor)"
  fi

  [[ "${worker}" == *@sha256:* ]] || die "worker image is not digest-pinned: ${worker}"
  mkdir -p "${STATE_DIR}"
  printf '%s\t%s\n' "${cache_key}" "${worker}" > "${cache_file}"
  WORKER_IMAGE="${worker}"
  export WORKER_IMAGE
}

resolve_factorio_image() {
  if [[ -n "${FACTORIO_IMAGE:-}" ]]; then
    export FACTORIO_IMAGE
    return 0
  fi
  local tag="${FACTORIO_TAG:-stable}"
  local digest=""
  digest="$(curl -fsSL "https://hub.docker.com/v2/repositories/factoriotools/factorio/tags/${tag}" 2>/dev/null | jq -r '.digest // empty' || true)"
  if [[ -z "${digest}" ]]; then
    digest="sha256:0ef394757b906f8e2e42d7e1f05332ad7cf7575c40d9b2412fa4c477189bdea7"
    warn "Could not fetch Docker Hub digest for factoriotools/factorio:${tag}; using pinned fallback ${digest}"
  fi
  FACTORIO_IMAGE="index.docker.io/factoriotools/factorio@${digest}"
  export FACTORIO_IMAGE
}

# resolve_worlds populates WORLDS (array), ACTOR_TEMPLATE, ROUTES_ARG from
# manifests/actors.yaml (overridden by WORLDS_LIST / ATESPACE / BASE_UDP_PORT if set).
resolve_worlds() {
  local actors_yaml="${REPO_ROOT}/manifests/actors.yaml"
  if [[ -f "${actors_yaml}" ]]; then
    local parsed
    parsed="$(python3 -c '
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1])) or {}
print(doc.get("atespace", "factorio"))
print(doc.get("template", "factorio-server"))
print(doc.get("baseUdpPort", 34197))
print(" ".join(a["name"] for a in doc.get("actors", [])))
' "${actors_yaml}")"
    local y_atespace y_tmpl y_port y_worlds
    {
      IFS= read -r y_atespace
      IFS= read -r y_tmpl
      IFS= read -r y_port
      IFS= read -r y_worlds
    } <<< "${parsed}"
    : "${ATESPACE:=${y_atespace}}"
    : "${ACTOR_TEMPLATE:=${y_tmpl}}"
    : "${BASE_UDP_PORT:=${y_port}}"
    if [[ -z "${WORLDS_LIST:-}" ]]; then
      WORLDS_LIST="${y_worlds}"
    fi
  fi

  if [[ -n "${WORLDS_LIST:-}" ]]; then
    read -r -a WORLDS <<< "${WORLDS_LIST}"
  else
    WORLDS=(world-vanilla world-deathworld world-railworld world-ribbonworld)
  fi
  : "${ACTOR_TEMPLATE:=factorio-server}"

  local idx port actor routes_items=()
  for idx in "${!WORLDS[@]}"; do
    port=$((BASE_UDP_PORT + idx))
    actor="${WORLDS[$idx]}"
    routes_items+=("${port}=${ATESPACE}/${actor}")
  done
  ROUTES_ARG="$(IFS=,; echo "${routes_items[*]}")"
  WORLDS_LIST="${WORLDS[*]}"
  export ATESPACE ACTOR_TEMPLATE BASE_UDP_PORT ROUTES_ARG WORLDS_LIST
}

render_computeclass_yaml() {
  python3 -c '
import os, sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
zones = [z.strip() for z in os.environ.get("WORKER_ZONES", "").split(",") if z.strip()]
if zones:
    doc.setdefault("spec", {}).setdefault("priorityDefaults", {}).setdefault("location", {})["zones"] = zones
yaml.safe_dump(doc, sys.stdout, sort_keys=False)
' "${REPO_ROOT}/manifests/computeclass.yaml"
}

render_workerpool_yaml() {
  [[ -n "${WORKER_IMAGE:-}" ]] || die "WORKER_IMAGE must be set before rendering workerpool.yaml"
  envsubst '${MAX_ACTIVE_WORLDS} ${WORKER_IMAGE}' < "${REPO_ROOT}/manifests/workerpool.yaml"
}

render_gateway_yaml() {
  resolve_worlds
  envsubst '${ROUTES_ARG} ${IDLE_TIMEOUT} ${PREEMPT_IDLE_GRACE} ${MAX_ACTIVE_WORLDS} ${WORKERPOOL_IDLE_TIMEOUT} ${CONTROL_POOL_NAME}' \
    < "${REPO_ROOT}/manifests/gateway.yaml" \
    | python3 -c '
import os, sys, yaml
worlds = os.environ.get("WORLDS_LIST", "").split()
base_port = int(os.environ.get("BASE_UDP_PORT", "34197"))
docs = list(yaml.safe_load_all(sys.stdin))
if worlds:
    udp_ports = [
        {
            "name": f"udp-port-{base_port + i}",
            "containerPort": base_port + i,
            "hostPort": base_port + i,
            "protocol": "UDP",
        }
        for i in range(len(worlds))
    ]
    for doc in docs:
        if isinstance(doc, dict) and doc.get("kind") == "Deployment":
            containers = doc["spec"]["template"]["spec"]["containers"]
            for c in containers:
                if c.get("name") == "gateway":
                    http_ports = [p for p in c.get("ports", []) if p.get("protocol") != "UDP"]
                    c["ports"] = udp_ports + http_ports
yaml.safe_dump_all(docs, sys.stdout, sort_keys=False)
'
}

render_actor_template_yaml() {
  [[ -n "${FACTORIO_IMAGE:-}" ]] || die "FACTORIO_IMAGE must be set before rendering actor-template.yaml"
  # Strict allowlist: never expand runtime ${TARGET_ACTOR} bash variables in the container entrypoint.
  envsubst '${FACTORIO_IMAGE}' < "${REPO_ROOT}/manifests/actor-template.yaml"
}

gw_external_ip() {
  local tries="${1:-60}" gw_node="" ip="" i
  for ((i = 1; i <= tries; i++)); do
    gw_node="$(kubectl get pod -n "${NAMESPACE}" -l app=factorio-gateway -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
    if [[ -n "${gw_node}" ]]; then
      ip="$(kubectl get node "${gw_node}" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)"
    fi
    if [[ -n "${ip}" ]]; then
      GW_NODE="${gw_node}"
      EXTERNAL_IP="${ip}"
      export GW_NODE EXTERNAL_IP
      return 0
    fi
    sleep 3
  done
  return 1
}

ensure_udp_firewall_rule() {
  local max_port=$((BASE_UDP_PORT + 100))
  if ! gcloud compute firewall-rules describe "${FIREWALL_RULE_NAME}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    step "Creating GCP firewall rule ${FIREWALL_RULE_NAME} (udp:${BASE_UDP_PORT}-${max_port})..."
    gcloud compute firewall-rules create "${FIREWALL_RULE_NAME}" \
      --project="${PROJECT_ID}" \
      --network=default \
      --allow="udp:${BASE_UDP_PORT}-${max_port}" \
      --source-ranges=0.0.0.0/0 \
      --description="Allow Factorio UDP traffic to node hostPorts ${BASE_UDP_PORT}-${max_port}" \
      >/dev/null
    ok "Firewall rule ${FIREWALL_RULE_NAME} ready"
  else
    ok "Firewall rule ${FIREWALL_RULE_NAME} already exists"
  fi
}
