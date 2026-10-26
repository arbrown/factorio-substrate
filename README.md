# Factorio on Agent Substrate (`factorio-ate`)

Running stateful, UDP-based dedicated game servers (`factoriotools/factorio:stable`) as on-demand, auto-suspending **Agent Substrate (`ate.dev/v1alpha1`) Actors** on GKE, with **multi-world multiplexing** over a single warm `WorkerPool` pod.

This repository is the companion implementation to the follow-up post on [Factorio on GKE with Spot VMs and Auto-Shutdown](https://wasd.drewbrown.me/posts/factorio-gke-spot/).

See [`TRADEOFFS.md`](TRADEOFFS.md) for a detailed breakdown of the 7 architectural challenges ("holes poked") and how this implementation resolves them without modifying Agent Substrate.

---

## Architecture Overview

```
Factorio Client (UDP :34197 Vanilla / :34198 Deathworld)
       │
       ▼  (GKE LoadBalancer Service, UDP 34197 & 34198)
┌──────────────────────────────────────────────────────────┐
│ factorio-gateway Pod (ate-system namespace, always-on)   │
│  1. Drops small/malformed internet UDP scanner packets   │
│  2. If target world is SUSPENDED:                        │
│     • Preempts & saves any other idle world on the pool  │
│     • Calls ate.v1alpha1.Actors.ResumeActor (mTLS gRPC)  │
│     • Buffers initial UDP handshake datagrams            │
│  3. Opens WebSocket tunnel via atenet-router (:8080/ws)  │
│     with header `ate-target-actor: factorio/<world>`     │
│  4. Tracks bidirectional UDP traffic; after 10m of       │
│     silence, triggers RCON `/server-save` + SuspendActor │
└──────────────────────────┬───────────────────────────────┘
                           │ WebSocket Frame: [2B addrLen][client IP:port][UDP payload]
                           ▼ (via atenet-router -> atelet -> Actor Pod netns)
┌──────────────────────────────────────────────────────────┐
│ Factorio Actor Sandbox (gVisor runsc + durableDir /factorio)
│  ┌────────────────────────┐   127.0.0.1:34197/UDP        │
│  │ factorio-sidecar (:80) │ ◄──────────────────────────► │
│  │  • /ws UDP bridge      │   127.0.0.1:27015/TCP (RCON) │
│  │  • /readyz UDP probe   │   ┌────────────────────────┐ │
│  │  • /prepare-suspend    │   │ factoriotools/factorio │ │
│  │  • /status RCON stats  │   │  Headless Server 2.1.x │ │
│  └────────────────────────┘   └────────────────────────┘ │
└──────────────────────────────────────────────────────────┘
```

### Multi-World Multiplexing on a Single Warm Worker
By default, we deploy:
- **1 warm `WorkerPool` pod** (`factorio-pool`, `replicas: 1`)
- **2 Factorio `Actor` worlds**:
  - `factorio/world-vanilla` (exposed on LoadBalancer UDP `:34197`)
  - `factorio/world-deathworld` (exposed on LoadBalancer UDP `:34198`)

Connecting to `:34197` wakes `world-vanilla` into the warm worker pod. Connecting to `:34198` while `world-vanilla` is idle automatically triggers an RCON `/server-save` on `world-vanilla`, snapshots its memory + `/factorio` directory to GCS (`gs://<BUCKET_NAME>`), and restores `world-deathworld` into the exact same worker pod!

---

## Repository Layout

- [`cmd/factorio-gateway/main.go`](cmd/factorio-gateway/main.go) — Always-on UDP-to-WebSocket ingress gateway, `ResumeActor`/`SuspendActor` lifecycle manager, single-worker multiplexer, and idle timer.
- [`cmd/factorio-sidecar/main.go`](cmd/factorio-sidecar/main.go) — In-actor sidecar that seeds `/factorio/config`, bridges `/ws` frames to `127.0.0.1:34197/UDP`, probes Factorio readiness via UDP `ConnectionRequest`, and triggers RCON `/server-save` on `/prepare-suspend`.
- [`cmd/factorio-probe/main.go`](cmd/factorio-probe/main.go) — CLI probe that sends real Factorio 2.x UDP `ConnectionRequest` packets (`0x42 0x02 0x01 0x17...`) to test wake-on-UDP and measure resume latency without needing a graphical Factorio client.
- [`internal/tunnelproto/frame.go`](internal/tunnelproto/frame.go) — Wire framing (`[2B addrLen][addr][payload]`) and Factorio UDP header validation.
- [`internal/rcon/rcon.go`](internal/rcon/rcon.go) — Minimal Source RCON TCP client for talking to Factorio on `127.0.0.1:27015`.
- [`manifests/`](manifests/) — Kubernetes (`ATEnamespace`, `WorkerPool`, `Deployment`, `Service`) and Agent Substrate (`ActorTemplate`) manifest templates.
- [`hack/deploy.sh`](hack/deploy.sh) — Builds images with `ko`, applies manifests, registers the `ActorTemplate`, and creates both Factorio `Actor` worlds (`world-vanilla` and `world-deathworld`).
- [`hack/test-e2e.sh`](hack/test-e2e.sh) — Automated end-to-end verification script testing wake-on-UDP, RCON status, multi-world multiplexing on the single worker pod, and idle auto-suspend.
- [`TRADEOFFS.md`](TRADEOFFS.md) — Explicit record of all architectural trade-offs and design decisions.

---

## Reproducible Step-by-Step Instructions

### Prerequisites
- `gcloud`, `kubectl`, `ko`, `go` (1.24+), `jq`, `curl`
- A local checkout of the Agent Substrate repository (sibling directory `../substrate` or set `SUBSTRATE_REPO=/path/to/substrate`)

### 1. Configure Environment
Copy `.ate-dev-env.sh.example` to `.ate-dev-env.sh` and set your GCP project and cluster details (defaults are pre-configured for `drewbr-sandbox-85` / `factorio-ate`):

```bash
cp .ate-dev-env.sh.example .ate-dev-env.sh
source .ate-dev-env.sh
```

### 2. Provision the GKE Cluster & Install Agent Substrate
From your Agent Substrate checkout (`$SUBSTRATE_REPO`), bootstrap the GKE cluster (`c3-standard-4` nodes for nested virtualization / gVisor support) and GCS snapshot bucket, then deploy `ate-system`:

```bash
cd "$SUBSTRATE_REPO"
go run ./tools/setup-gcp bootstrap \
  --project "$PROJECT_ID" \
  --cluster "$CLUSTER_NAME" \
  --bucket "$BUCKET_NAME" \
  --zone "$CLUSTER_LOCATION"

# Disable node pool auto-upgrade so the custom atelet DaemonSet stays version-aligned
gcloud container node-pools update substrate-node-pool \
  --cluster="$CLUSTER_NAME" \
  --zone="$CLUSTER_LOCATION" \
  --project="$PROJECT_ID" \
  --no-enable-autoupgrade

# Build and deploy Agent Substrate control plane + atelet DaemonSet
ATE_ENV_FILE="$(pwd)/../factorio-substrate/.ate-dev-env.sh" ./hack/install-ate.sh --deploy-ate-system
```

### 3. Deploy Factorio Gateway, Sidecar, WorkerPool, and Worlds
Return to `factorio-substrate` and run `make deploy` (or `./hack/deploy.sh`):

```bash
cd /path/to/factorio-substrate
make deploy
```

This will:
1. Build and push `factorio-sidecar` and `factorio-gateway` to `$KO_DOCKER_REPO`.
2. Create the `factorio` `ATEnamespace` and 1-replica `factorio-pool` `WorkerPool`.
3. Deploy `factorio-gateway` and expose it via the `factorio-gateway-udp` `LoadBalancer` Service (UDP ports `34197` and `34198`).
4. Register the `factorio-server` `ActorTemplate` (`SNAPSHOT_CONTENT_SCOPE_FULL` + `/factorio` `durableDir`).
5. Create `factorio/world-vanilla` and `factorio/world-deathworld` and suspend both into GCS so they start in `ACTOR_STATE_SUSPENDED` (zero idle compute used by Factorio).

### 4. Verify End-to-End (Headless Probe + Multiplexing)
You can verify the entire lifecycle without a graphical Factorio client using `make verify` (or `./hack/test-e2e.sh`):

```bash
make verify
```

To also verify the automatic idle-timeout RCON save + suspend cycle using a shortened 30-second timeout (and automatically restore the 10-minute timeout afterward):

```bash
make verify-with-idle
```

### 5. Connect with a Real Factorio Client
Find the external LoadBalancer IP:

```bash
kubectl -n ate-system get svc factorio-gateway-udp
```

In the Factorio 2.1.x game client (**Multiplayer -> Connect to address**):
- **Vanilla World:** `<EXTERNAL-IP>:34197`
- **Deathworld:** `<EXTERNAL-IP>:34198`

*(Note: If the world is currently suspended, the first connection attempt triggers `ResumeActor` in the background; if the Factorio client times out while the world restores, simply click **Connect** a second time once the world is awake.)*
