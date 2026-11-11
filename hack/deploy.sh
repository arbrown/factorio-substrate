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

# Deploys the Factorio workload on an already-provisioned Agent Substrate cluster:
#   1. Namespace & RBAC (`manifests/namespace.yaml`, `manifests/rbac.yaml`)
#   2. Multi-zone Spot ComputeClass (`manifests/computeclass.yaml`)
#   3. WorkerPool (`manifests/workerpool.yaml`)
#   4. UDP Gateway Deployment & HTTP Status Service (`manifests/gateway.yaml`)
#   5. `factorio-server` ActorTemplate & Golden Snapshot (`manifests/actor-template.yaml`)
#   6. `factorio` Atespace & world Actors (`manifests/actors.yaml`)
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_env

"${REPO_ROOT}/hack/deploy-k8s.sh" all
"${REPO_ROOT}/hack/deploy-actors.sh" all
