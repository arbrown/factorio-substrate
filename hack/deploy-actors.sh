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

# Registers Agent Substrate Atespace, ActorTemplate, and world Actors:
#   hack/deploy-actors.sh [template|actors|all]
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

ensure_atespace() {
  if ! run_kubectl_ate get atespace "${ATESPACE}" >/dev/null 2>&1; then
    step "Creating '${ATESPACE}' Atespace"
    run_kubectl_ate create atespace "${ATESPACE}"
    ok "Atespace '${ATESPACE}' created"
  fi
}

deploy_template() {
  ensure_kubectl_ate
  ensure_ko
  resolve_worlds
  ensure_atespace

  step "Resolving factoriotools/factorio:${FACTORIO_TAG} digest..."
  resolve_factorio_image
  ok "FACTORIO_IMAGE=${FACTORIO_IMAGE}"

  step "Rendering '${ACTOR_TEMPLATE}' ActorTemplate (manifests/actor-template.yaml + ko resolve)..."
  local rendered_template new_sidecar_image
  rendered_template="$(render_actor_template_yaml | run_ko resolve -f -)"
  new_sidecar_image="$(echo "${rendered_template}" | grep -A2 'name: sidecar' | awk '/image:/ {print $2}')"

  if run_kubectl_ate get actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" >/dev/null 2>&1; then
    local tmpl_json snap_uri cur_factorio_image old_uid
    tmpl_json="$(run_kubectl_ate get actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" -o json)"
    snap_uri="$(echo "${tmpl_json}" | jq -r '.status.goldenSnapshotStatus.goldenTag.name // empty')"
    cur_factorio_image="$(echo "${tmpl_json}" | jq -r '.containers[] | select(.name=="factorio") | .image // empty')"
    if [[ -n "${snap_uri}" && "${cur_factorio_image}" == "${FACTORIO_IMAGE}" && "${RECREATE_TEMPLATE:-false}" != "true" ]]; then
      ok "ActorTemplate ${ATESPACE}/${ACTOR_TEMPLATE} up to date (factorio=${cur_factorio_image})"
      return 0
    fi
  fi

  local gw_prev_replicas="0"
  gw_prev_replicas="$(kubectl get deployment -n "${NAMESPACE}" factorio-gateway -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)"
  gw_prev_replicas="${gw_prev_replicas:-0}"
  restore_gateway_after_template() {
    if [[ "${gw_prev_replicas}" -gt 0 ]]; then
      step "Restoring factorio-gateway to ${gw_prev_replicas} replica(s)"
      kubectl scale deployment/factorio-gateway -n "${NAMESPACE}" --replicas="${gw_prev_replicas}" >/dev/null 2>&1 || true
      kubectl rollout status deployment/factorio-gateway -n "${NAMESPACE}" --timeout=180s >/dev/null 2>&1 || true
    fi
  }
  trap 'trap - RETURN; restore_gateway_after_template' RETURN

  if [[ "${gw_prev_replicas}" -gt 0 ]]; then
    step "Temporarily pausing factorio-gateway so WorkerPool/factorio stays warm during Golden Snapshot build"
    kubectl scale deployment/factorio-gateway -n "${NAMESPACE}" --replicas=0 >/dev/null
    kubectl wait --for=delete pod -l app=factorio-gateway -n "${NAMESPACE}" --timeout=120s >/dev/null 2>&1 || true
  fi

  if kubectl get workerpool factorio -n "${NAMESPACE}" >/dev/null 2>&1; then
    local wp_replicas
    wp_replicas="$(kubectl get workerpool factorio -n "${NAMESPACE}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)"
    if [[ "${wp_replicas:-0}" -lt 1 ]]; then
      step "Scaling WorkerPool/factorio to ${MAX_ACTIVE_WORLDS} for Golden Snapshot build"
      kubectl patch workerpool factorio -n "${NAMESPACE}" --type=merge \
        -p "{\"spec\":{\"replicas\":${MAX_ACTIVE_WORLDS}}}" >/dev/null
    fi
    kubectl rollout status "deployment/factorio" -n "${NAMESPACE}" --timeout=360s
  fi

  if run_kubectl_ate get actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" >/dev/null 2>&1; then
    local tmpl_json old_uid
    tmpl_json="$(run_kubectl_ate get actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" -o json)"
    old_uid="$(echo "${tmpl_json}" | jq -r '.metadata.uid // empty')"
    step "Recreating ${ATESPACE}/${ACTOR_TEMPLATE} (factorio=${FACTORIO_IMAGE}, sidecar=${new_sidecar_image})..."
    for actor in "${WORLDS[@]}"; do
      run_kubectl_ate suspend actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
    done
    run_kubectl_ate delete actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" || true
    if [[ -n "${old_uid}" ]]; then
      kubectl exec -n ate-system postgres-0 -c postgres -- psql -U postgres -d atepg \
        -c "DELETE FROM worker_assignments; DELETE FROM workers; DELETE FROM actors WHERE name = '${old_uid}';" >/dev/null 2>&1 || true
      kubectl delete pod -n "${NAMESPACE}" -l ate.dev/worker-pool=factorio --wait=false >/dev/null 2>&1 || true
      kubectl rollout status deployment/factorio -n "${NAMESPACE}" --timeout=180s
    fi
    sleep 2
  fi

  if ! run_kubectl_ate get actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" >/dev/null 2>&1; then
    echo "${rendered_template}" | run_kubectl_ate create actor-template -f -
  fi

  step "Waiting for '${ACTOR_TEMPLATE}' Golden Snapshot to become Ready..."
  local i status err_msg
  for i in $(seq 1 90); do
    status="$(run_kubectl_ate get actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" -o json 2>/dev/null || true)"
    if echo "${status}" | jq -e '.status.goldenSnapshotStatus.goldenTag.name | length > 0' >/dev/null 2>&1; then
      ok "Golden Snapshot Ready"
      return 0
    fi
    err_msg="$(echo "${status}" | jq -r '.status.goldenSnapshotStatus.errorMessage // empty' 2>/dev/null || true)"
    if [[ -n "${err_msg}" ]]; then
      echo "${status}" >&2
      die "ActorTemplate ${ATESPACE}/${ACTOR_TEMPLATE} failed: ${err_msg}"
    fi
    sleep 5
  done
  echo "${status}" >&2
  die "timed out waiting for Golden Snapshot on ${ATESPACE}/${ACTOR_TEMPLATE}"
}

deploy_actors() {
  ensure_kubectl_ate
  resolve_worlds
  ensure_atespace

  run_kubectl_ate get actor-template "${ACTOR_TEMPLATE}" -a "${ATESPACE}" >/dev/null 2>&1 \
    || die "ActorTemplate ${ATESPACE}/${ACTOR_TEMPLATE} not found — run 'make actor-template' first"

  if [[ "${RESET_WORLDS:-false}" == "true" ]]; then
    step "RESET_WORLDS=true: suspending and deleting existing world Actors so they regenerate fresh maps"
    for actor in "${WORLDS[@]}"; do
      kubectl create --raw "/api/v1/namespaces/${NAMESPACE}/services/factorio-gateway-http:status-http/proxy/suspend?actor=${ATESPACE}/${actor}" -f - <<< '{}' >/dev/null 2>&1 || true
      run_kubectl_ate suspend actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
      run_kubectl_ate delete actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
    done
  fi

  step "Reconciling ${#WORLDS[@]} world Actors in atespace '${ATESPACE}' (manifests/actors.yaml)..."
  local existing_actors
  existing_actors="$(run_kubectl_ate get actors -a "${ATESPACE}" -o json 2>/dev/null | jq -r '.actors[]?.metadata.name // empty' || true)"

  if [[ "${PRUNE_EXTRA_WORLDS:-true}" == "true" && -n "${existing_actors}" ]]; then
    local want_actors prune_batch=0 existing
    want_actors="$(printf "%s\n" "${WORLDS[@]}")"
    for existing in ${existing_actors}; do
      if ! grep -qx "${existing}" <<< "${want_actors}"; then
        warn "Pruning extra Actor '${existing}' not in WORLDS_LIST"
        (
          run_kubectl_ate suspend actor "${existing}" -a "${ATESPACE}" >/dev/null 2>&1 || true
          run_kubectl_ate delete actor "${existing}" -a "${ATESPACE}" >/dev/null 2>&1 || true
        ) &
        prune_batch=$((prune_batch + 1))
        if (( prune_batch >= 8 )); then
          wait
          prune_batch=0
        fi
      fi
    done
    wait
  fi

  local batch=0 actor
  for actor in "${WORLDS[@]}"; do
    if ! grep -qx "${actor}" <<< "${existing_actors}"; then
      run_kubectl_ate create actor "${actor}" -a "${ATESPACE}" --template "${ACTOR_TEMPLATE}" >/dev/null &
      batch=$((batch + 1))
      if (( batch >= 8 )); then
        wait
        batch=0
      fi
    fi
  done
  wait

  step "Current Actors in '${ATESPACE}' atespace:"
  run_kubectl_ate get actor -a "${ATESPACE}"

  GW_NODE=""
  EXTERNAL_IP=""
  gw_external_ip 5 >/dev/null 2>&1 || true
  echo "================================================================================"
  echo " Factorio on Agent Substrate Deployed (Spot Nodes + hostPort Ingress)"
  echo " Gateway Node:        ${GW_NODE:-<pending>}"
  echo " External Host IP:    ${EXTERNAL_IP:-<pending>}"
  echo ""
  echo " Active World Routes (UDP hostPorts):"
  for idx in "${!WORLDS[@]}"; do
    port=$((BASE_UDP_PORT + idx))
    actor="${WORLDS[$idx]}"
    echo "   - ${EXTERNAL_IP:-<NODE_IP>}:${port}  ==>  ${ATESPACE}/${actor}"
  done
  echo "================================================================================"
}

TARGET="${1:-all}"
case "${TARGET}" in
  template) deploy_template ;;
  actors)   deploy_actors ;;
  all)
    deploy_template
    deploy_actors
    ;;
  *)
    die "usage: hack/deploy-actors.sh [template|actors|all]"
    ;;
esac
