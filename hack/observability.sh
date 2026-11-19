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

# Configures GKE Cloud Logging, Cloud Monitoring, Managed Prometheus, and
# Agent Substrate (`ate-api-server`) log verbosity using first-party flags.
# Usage: hack/observability.sh [apply|off|on|status]
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

_explicit_api_log_level="${ATE_API_LOG_LEVEL:-}"
load_env

ACTION="${1:-apply}"
case "${ACTION}" in
  apply) ;;
  off)
    ATE_API_LOG_LEVEL="${_explicit_api_log_level:-error}"
    GKE_LOGGING="SYSTEM"
    GKE_MONITORING="SYSTEM"
    GKE_MANAGED_PROMETHEUS="false"
    ;;
  on)
    ATE_API_LOG_LEVEL="${_explicit_api_log_level:-info}"
    GKE_LOGGING="SYSTEM,WORKLOAD"
    GKE_MONITORING="SYSTEM,STORAGE,HPA,POD,DAEMONSET,DEPLOYMENT,STATEFULSET,CADVISOR,KUBELET,DCGM,JOBSET"
    GKE_MANAGED_PROMETHEUS="true"
    ;;
  status) ;;
  *)
    die "usage: hack/observability.sh [apply|off|on|status]"
    ;;
esac

_normalize_components() {
  local raw="${1:-}"
  if [[ -z "${raw}" ]]; then
    echo "NONE"
    return 0
  fi
  printf '%s\n' "${raw}" \
    | tr -d ' \t\r' \
    | tr ';,' '\n' \
    | tr '[:lower:]' '[:upper:]' \
    | sed -e 's/^SYSTEM_COMPONENTS$/SYSTEM/' -e 's/^WORKLOADS$/WORKLOAD/' -e 's/^APISERVER$/API_SERVER/' \
    | grep -v '^$' \
    | sort -u \
    | paste -sd, -
}

_normalize_bool() {
  case "${1:-}" in
    true|True|TRUE|1) echo "true" ;;
    *) echo "false" ;;
  esac
}

describe_cluster_observability() {
  local info_json parsed
  info_json="$(gcloud container clusters describe "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" \
    --format='json(loggingConfig.componentConfig.enableComponents,monitoringConfig.componentConfig.enableComponents,monitoringConfig.managedPrometheusConfig.enabled)' 2>/dev/null || true)"
  [[ -n "${info_json}" ]] || die "failed to describe cluster ${CLUSTER_NAME} (${CLUSTER_LOCATION}) in ${PROJECT_ID}"
  parsed="$(printf '%s\n' "${info_json}" | jq -r '
    [
      ((.loggingConfig.componentConfig.enableComponents // []) | join(",")),
      ((.monitoringConfig.componentConfig.enableComponents // []) | join(",")),
      ((.monitoringConfig.managedPrometheusConfig.enabled // false) | tostring)
    ] | .[]
  ')"
  local _cur_log_raw _cur_mon_raw _cur_prom_raw
  {
    IFS= read -r _cur_log_raw
    IFS= read -r _cur_mon_raw
    IFS= read -r _cur_prom_raw
  } <<< "${parsed}"
  CUR_GKE_LOGGING="$(_normalize_components "${_cur_log_raw:-}")"
  CUR_GKE_MONITORING="$(_normalize_components "${_cur_mon_raw:-}")"
  CUR_GKE_MANAGED_PROMETHEUS="$(_normalize_bool "${_cur_prom_raw:-}")"
}

if [[ "${ACTION}" == "status" ]]; then
  describe_cluster_observability
  echo "Cluster: ${CLUSTER_NAME} (${CLUSTER_LOCATION}) in ${PROJECT_ID}"
  echo "  GKE Logging:            ${CUR_GKE_LOGGING}"
  echo "  GKE Monitoring:         ${CUR_GKE_MONITORING}"
  echo "  GKE Managed Prometheus: ${CUR_GKE_MANAGED_PROMETHEUS}"
  if kubectl get cm -n ate-system ate-api-config >/dev/null 2>&1; then
    api_cfg="$(kubectl get cm -n ate-system ate-api-config -o jsonpath='{.data.apiconfig\.textproto}' 2>/dev/null | tr '\n' ' ' | sed -E 's/[[:space:]]+/ /g; s/[[:space:]]+$//' || true)"
    echo "  ate-api-config:         ${api_cfg:-<empty>}"
  else
    echo "  ate-api-config:         <cluster unreachable or ate-system not installed>"
  fi
  exit 0
fi

case "${ATE_API_LOG_LEVEL}" in
  debug|info|warn|error) ;;
  *) die "invalid ATE_API_LOG_LEVEL=${ATE_API_LOG_LEVEL} (expected debug, info, warn, or error)" ;;
esac

DESIRED_LOGGING="$(_normalize_components "${GKE_LOGGING}")"
DESIRED_MONITORING="$(_normalize_components "${GKE_MONITORING}")"
DESIRED_PROMETHEUS="$(_normalize_bool "${GKE_MANAGED_PROMETHEUS}")"

step "Configuring GKE observability (${ACTION}: logging=${DESIRED_LOGGING}, monitoring=${DESIRED_MONITORING}, managed-prometheus=${DESIRED_PROMETHEUS})"
describe_cluster_observability

if [[ "${CUR_GKE_LOGGING}" == "${DESIRED_LOGGING}" && \
      "${CUR_GKE_MONITORING}" == "${DESIRED_MONITORING}" && \
      "${CUR_GKE_MANAGED_PROMETHEUS}" == "${DESIRED_PROMETHEUS}" ]]; then
  ok "GKE cluster observability already matches target state"
else
  update_args=(
    --zone="${CLUSTER_LOCATION}"
    --project="${PROJECT_ID}"
    --logging="${DESIRED_LOGGING}"
    --monitoring="${DESIRED_MONITORING}"
  )
  if [[ "${DESIRED_PROMETHEUS}" == "true" ]]; then
    update_args+=(--enable-managed-prometheus)
  else
    update_args+=(--disable-managed-prometheus)
  fi
  gcloud container clusters update "${CLUSTER_NAME}" "${update_args[@]}" --quiet
  ok "Updated GKE logging/monitoring/Managed Prometheus on ${CLUSTER_NAME}"
fi

if kubectl get deployment -n ate-system ate-api-server >/dev/null 2>&1; then
  step "Configuring ate-api-server log level (${ATE_API_LOG_LEVEL}) via ate-setup"
  cur_api_cfg="$(kubectl get cm -n ate-system ate-api-config -o jsonpath='{.data.apiconfig\.textproto}' 2>/dev/null || true)"
  if printf '%s\n' "${cur_api_cfg}" | grep -Eq "level:[[:space:]]*\"${ATE_API_LOG_LEVEL}\""; then
    ok "ate-api-server log level already ${ATE_API_LOG_LEVEL}"
  else
    ensure_substrate_checkout
    ate_ctx_arg=()
    if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
      ate_ctx_arg=(--context="${KUBECTL_CONTEXT}")
    elif cur_ctx="$(kubectl config current-context 2>/dev/null || true)" && [[ -n "${cur_ctx}" ]]; then
      ate_ctx_arg=(--context="${cur_ctx}")
    fi
    (cd "${SUBSTRATE_SRC}" && go run ./cmd/ate-setup create api-config \
      --no-dev-env \
      "${ate_ctx_arg[@]}" \
      --gcp-project-id="${PROJECT_ID}" \
      --gcp-cluster-location="${CLUSTER_LOCATION}" \
      --gcp-cluster-name="${CLUSTER_NAME}" \
      --ateapi-log-level="${ATE_API_LOG_LEVEL}")
    kubectl rollout status deployment/ate-api-server -n ate-system --timeout=180s
    ok "ate-api-server rolled onto log level ${ATE_API_LOG_LEVEL}"
  fi
else
  warn "ate-system/ate-api-server not deployed yet; skipping ate-api-config update"
fi
