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

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh ]]; then
  source .ate-dev-env.sh
fi

PROJECT_ID="${PROJECT_ID:-drewbr-sandbox-85}"
CLUSTER_NAME="${CLUSTER_NAME:-factorio-ate}"
CLUSTER_LOCATION="${CLUSTER_LOCATION:-us-central1-a}"

echo "================================================================================"
echo " Setting up Cost-Optimized Node Pools (Option A Scale-to-Zero)"
echo " Cluster:  ${CLUSTER_NAME} (${CLUSTER_LOCATION}) in project ${PROJECT_ID}"
echo "================================================================================"

SUBSTRATE_VERSION="$(kubectl get ds -n ate-system -l app=atelet -o jsonpath='{.items[0].metadata.labels.ate\.dev/substrate-version}')"
if [[ -z "${SUBSTRATE_VERSION}" ]]; then
  echo "error: could not find atelet DaemonSet in namespace ate-system" >&2
  exit 1
fi
echo "==> Substrate version: ${SUBSTRATE_VERSION}"

# 1. Back up postgres data before touching storage.
echo "==> Backing up postgres database 'atepg'..."
mkdir -p /tmp/factorio-substrate-backup
kubectl exec -n ate-system postgres-0 -c postgres -- pg_dump -U postgres atepg > /tmp/factorio-substrate-backup/atepg.sql
if [[ ! -s /tmp/factorio-substrate-backup/atepg.sql ]]; then
  echo "error: postgres backup is empty" >&2
  exit 1
fi
echo "    Postgres backup saved ($(wc -l < /tmp/factorio-substrate-backup/atepg.sql) lines)"

# 2. Update cluster autoscaling profile to optimize-utilization for rapid scale-down.
echo "==> Updating cluster autoscaling profile to 'optimize-utilization'..."
gcloud container clusters update "${CLUSTER_NAME}" \
  --zone "${CLUSTER_LOCATION}" \
  --project "${PROJECT_ID}" \
  --autoscaling-profile=optimize-utilization

# 3. Ensure 'substrate-worker-pool' is Spot (0 -> 1 n2-standard-2 Ice Lake, tainted, scale-to-zero).
if gcloud container node-pools describe substrate-worker-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  WORKER_IS_SPOT="$(gcloud container node-pools describe substrate-worker-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" --format='value(config.spot)' 2>/dev/null || echo "")"
  if [[ "${WORKER_IS_SPOT}" != "True" ]]; then
    echo "==> Existing 'substrate-worker-pool' is not Spot. Deleting to recreate as Spot..."
    gcloud container node-pools delete substrate-worker-pool \
      --cluster "${CLUSTER_NAME}" \
      --zone "${CLUSTER_LOCATION}" \
      --project "${PROJECT_ID}" \
      --quiet
  else
    echo "==> 'substrate-worker-pool' already exists and is Spot"
  fi
fi

if ! gcloud container node-pools describe substrate-worker-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  echo "==> Creating Spot 'substrate-worker-pool' (0 -> 1 n2-standard-2 Ice Lake, scale-to-zero)..."
  gcloud container node-pools create substrate-worker-pool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --project "${PROJECT_ID}" \
    --spot \
    --machine-type=n2-standard-2 \
    --min-cpu-platform="Intel Ice Lake" \
    --disk-size=50GB \
    --disk-type=pd-balanced \
    --enable-autoscaling \
    --min-nodes=0 \
    --max-nodes=1 \
    --node-labels="ate.dev/substrate-version=${SUBSTRATE_VERSION}" \
    --node-taints="ate.dev/sandboxClass=gvisor:NoSchedule"
fi

# 4. Ensure 'control-pool' is Spot (1x e2-medium, 30GB pd-standard).
if gcloud container node-pools describe control-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  CONTROL_IS_SPOT="$(gcloud container node-pools describe control-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" --format='value(config.spot)' 2>/dev/null || echo "")"
  if [[ "${CONTROL_IS_SPOT}" != "True" ]]; then
    echo "==> Existing 'control-pool' is not Spot. Migrating control plane to Spot..."
    echo "    Deleting non-Spot 'control-pool'..."
    gcloud container node-pools delete control-pool \
      --cluster "${CLUSTER_NAME}" \
      --zone "${CLUSTER_LOCATION}" \
      --project "${PROJECT_ID}" \
      --quiet

    echo "    Recreating Spot 'control-pool' (1x e2-medium)..."
    gcloud container node-pools create control-pool \
      --cluster "${CLUSTER_NAME}" \
      --zone "${CLUSTER_LOCATION}" \
      --project "${PROJECT_ID}" \
      --spot \
      --machine-type=e2-medium \
      --disk-size=30GB \
      --disk-type=pd-standard \
      --num-nodes=1 \
      --tags=factorio-gateway-node \
      --no-enable-autoupgrade

    echo "    Waiting for postgres-0 and ate-api-server to be Ready on Spot control-pool..."
    kubectl wait --for=condition=ready pod/postgres-0 -n ate-system --timeout=300s
    kubectl rollout status deployment/ate-api-server -n ate-system --timeout=180s
  else
    echo "==> 'control-pool' already exists and is Spot"
  fi
else
  echo "==> Creating Spot 'control-pool' (1x e2-medium, 30GB pd-standard)..."
  gcloud container node-pools create control-pool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --project "${PROJECT_ID}" \
    --spot \
    --machine-type=e2-medium \
    --disk-size=30GB \
    --disk-type=pd-standard \
    --num-nodes=1 \
    --tags=factorio-gateway-node \
    --no-enable-autoupgrade

  echo "    Waiting for postgres-0 and ate-api-server to be Ready on Spot control-pool..."
  kubectl wait --for=condition=ready pod/postgres-0 -n ate-system --timeout=300s
  kubectl rollout status deployment/ate-api-server -n ate-system --timeout=180s
fi

# Ensure GCP Firewall Rule allows UDP hostPorts (34197-34297).
echo "==> Ensuring GCP Firewall Rule allows UDP hostPorts (34197-34297)..."
if ! gcloud compute firewall-rules describe factorio-ate-allow-udp-hostport --project="${PROJECT_ID}" >/dev/null 2>&1; then
  echo "    Creating firewall rule factorio-ate-allow-udp-hostport..."
  gcloud compute firewall-rules create factorio-ate-allow-udp-hostport \
    --project="${PROJECT_ID}" \
    --network=default \
    --allow=udp:34197-34297 \
    --source-ranges=0.0.0.0/0 \
    --description="Allow Factorio UDP traffic to node hostPorts 34197-34297"
else
  echo "    Firewall rule factorio-ate-allow-udp-hostport already exists"
fi

# Remove legacy LoadBalancer Service if present.
if kubectl get svc -n factorio factorio-gateway >/dev/null 2>&1; then
  echo "==> Removing legacy LoadBalancer Service if present (using hostPort ingress)..."
  kubectl delete svc -n factorio factorio-gateway --ignore-not-found
fi

# Cordon old 'substrate-node-pool' nodes immediately so any recreated workloads land on 'control-pool'.
if gcloud container node-pools describe substrate-node-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  echo "==> Cordoning old substrate-node-pool nodes to redirect workloads to control-pool..."
  for node in $(kubectl get nodes -l cloud.google.com/gke-nodepool=substrate-node-pool -o jsonpath='{.items[*].metadata.name}'); do
    kubectl cordon "${node}" || true
  done
fi

# 5. Annotate atelet DaemonSet so Cluster Autoscaler can evict it during scale-to-zero.
echo "==> Annotating atelet DaemonSet with safe-to-evict..."
kubectl patch ds -n ate-system "atelet-${SUBSTRATE_VERSION}" \
  -p '{"spec":{"template":{"metadata":{"annotations":{"cluster-autoscaler.kubernetes.io/safe-to-evict":"true"}}}}}' || true

# 6. Scale ate-api-server to 1 replica to conserve resources on control-pool.
echo "==> Scaling ate-api-server to 1 replica..."
kubectl scale deployment/ate-api-server -n ate-system --replicas=1

# 7. Right-size postgres storage from 500Gi to 10Gi and requests to 25m/128Mi.
CURRENT_STORAGE="$(kubectl get pvc -n ate-system data-postgres-0 -o jsonpath='{.spec.resources.requests.storage}' 2>/dev/null || echo "")"
if [[ "${CURRENT_STORAGE}" == "500Gi" ]]; then
  echo "==> Right-sizing postgres from 500Gi to 10Gi..."
  kubectl delete sts -n ate-system postgres --wait=true
  kubectl delete pvc -n ate-system data-postgres-0 --wait=true

  cat <<'EOF' | kubectl apply -f -
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: ate-system
  labels:
    app: postgres
spec:
  replicas: 1
  serviceName: postgres
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: postgres:18-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15
        args: ["-c", "config_file=/etc/postgresql/postgresql.conf"]
        env:
        - name: POSTGRES_DB
          value: atepg
        - name: POSTGRES_HOST_AUTH_METHOD
          value: trust
        - name: PGDATA
          value: /var/lib/postgresql/data/pgdata
        lifecycle:
          postStart:
            exec:
              command:
              - /bin/sh
              - -ec
              - |
                until psql -U postgres -d postgres -Atc 'SELECT 1' >/dev/null 2>&1; do
                  sleep 1
                done
                if ! psql -U postgres -d postgres -Atc \
                  "SELECT 1 FROM pg_database WHERE datname = 'atepg'" | grep -qx 1; then
                  createdb -U postgres atepg
                fi
        livenessProbe:
          exec:
            command: ["pg_isready", "-U", "postgres", "-d", "postgres"]
          initialDelaySeconds: 10
          periodSeconds: 10
        readinessProbe:
          exec:
            command: ["/bin/sh", "-ec", "psql -U postgres -d atepg -Atc 'SELECT 1' >/dev/null"]
          initialDelaySeconds: 2
          periodSeconds: 2
        resources:
          requests:
            cpu: 25m
            memory: 128Mi
          limits:
            cpu: "1"
            memory: 512Mi
        volumeMounts:
        - name: config
          mountPath: /etc/postgresql
        - name: servicedns
          mountPath: /run/servicedns.podcert.ate.dev
          readOnly: true
        - name: podidentity-ca
          mountPath: /run/podidentity.podcert.ate.dev
          readOnly: true
        - name: socket
          mountPath: /var/run/postgresql
        - name: data
          mountPath: /var/lib/postgresql/data
      initContainers:
      - name: tls-reloader
        image: postgres:18-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15
        command: ["/bin/sh", "/etc/postgresql/reload-tls.sh"]
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
        restartPolicy: Always
        securityContext:
          runAsUser: 70
        volumeMounts:
        - name: config
          mountPath: /etc/postgresql
        - name: servicedns
          mountPath: /run/servicedns.podcert.ate.dev
          readOnly: true
        - name: podidentity-ca
          mountPath: /run/podidentity.podcert.ate.dev
          readOnly: true
        - name: socket
          mountPath: /var/run/postgresql
      securityContext:
        fsGroup: 70
        fsGroupChangePolicy: OnRootMismatch
      volumes:
      - name: config
        configMap:
          name: postgres-config
      - name: servicedns
        projected:
          sources:
          - podCertificate:
              signerName: servicedns.podcert.ate.dev/identity
              keyType: ECDSAP256
              credentialBundlePath: credential-bundle.pem
      - name: socket
        emptyDir: {}
      - name: podidentity-ca
        projected:
          sources:
          - clusterTrustBundle:
              signerName: podidentity.podcert.ate.dev/identity
              labelSelector:
                matchLabels:
                  podcert.ate.dev/canarying: live
              path: trust-bundle.pem
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 10Gi
EOF

  echo "==> Waiting for postgres-0 to be Ready on new 10Gi volume..."
  kubectl wait --for=condition=ready pod/postgres-0 -n ate-system --timeout=180s

  echo "==> Restoring 'atepg' database data..."
  kubectl exec -i -n ate-system postgres-0 -c postgres -- psql -U postgres -d atepg < /tmp/factorio-substrate-backup/atepg.sql
  echo "    Database restored successfully."
fi

# 8. If old 'substrate-node-pool' exists, cordon/drain and delete it.
if gcloud container node-pools describe substrate-node-pool --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  echo "==> Cordoning old substrate-node-pool nodes..."
  for node in $(kubectl get nodes -l cloud.google.com/gke-nodepool=substrate-node-pool -o jsonpath='{.items[*].metadata.name}'); do
    kubectl cordon "${node}" || true
  done

  echo "==> Draining old substrate-node-pool nodes to move workloads to control-pool..."
  for node in $(kubectl get nodes -l cloud.google.com/gke-nodepool=substrate-node-pool -o jsonpath='{.items[*].metadata.name}'); do
    kubectl drain "${node}" --delete-emptydir-data --ignore-daemonsets --force --timeout=180s || true
  done

  echo "==> Deleting old substrate-node-pool (2x c3-standard-4)..."
  gcloud container node-pools delete substrate-node-pool \
    --cluster "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --project "${PROJECT_ID}" \
    --quiet
fi

echo "================================================================================"
echo " Cost-optimized node pool migration complete!"
echo " Nodes:"
kubectl get nodes -o wide
echo "================================================================================"
