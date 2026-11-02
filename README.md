# Factorio on Agent Substrate (`factorio-ate`)

Running stateful, UDP-based dedicated game servers (`factoriotools/factorio:stable`) as on-demand, auto-suspending **Agent Substrate (`ate.dev/v1alpha1`) Actors** on GKE, with **multi-world multiplexing** over a single warm `WorkerPool` pod.

This repository is the companion implementation to the follow-up post on [Factorio on GKE with Spot VMs and Auto-Shutdown](https://wasd.drewbrown.me/posts/factorio-gke-spot/).

See [`TRADEOFFS.md`](TRADEOFFS.md) for a detailed breakdown of the 11 architectural challenges ("holes poked") and how this implementation resolves them without modifying Agent Substrate.

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

### Automatic Zero-Touch Factorio Version Upgrades (Proactive + Reactive)
Because Steam auto-updates players' Factorio clients whenever Wube releases a new `stable` patch, `factorio-gateway` implements a **dual auto-upgrade system**:
1. **Proactive Background Docker Hub Watcher (`--image-poll-interval=30m`):** Periodically polls Docker Hub (`factoriotools/factorio:stable`) while worlds are `SUSPENDED`. When the `@sha256:...` digest changes, `factorio-gateway` replaces the `factorio/factorio-server` `ActorTemplate` via mTLS gRPC (`GetActorTemplate` → `DeleteActorTemplate` → `CreateActorTemplate`) so the new Golden Snapshot is pre-built in the background.
2. **Reactive Client Version Sniffer on UDP `ConnectionRequest`:** Parses the connecting player's 4-byte Factorio version (`major.minor.patch.0`, e.g. `2.1.24.0`) from the first UDP `ConnectionRequest` (`0x02`) packet. If a player's Steam client has updated to a newer version before the 30-minute background poll has fired, `factorio-gateway` immediately checks Docker Hub and upgrades `factorio/factorio-server` on the spot.
3. **In-Place Save Migration via `SNAPSHOT_SCOPE_DATA`:** Existing `Actor`s (`world-vanilla`, `world-deathworld`) are **not** recreated. When an Actor wakes up after its `ActorTemplate` UID has changed, Agent Substrate's `TemplateReplaced` check (`workflow_resume.go`) automatically switches that single wake to `SNAPSHOT_SCOPE_DATA` (`RESUME_SOURCE_COLD_BOOT`), extracting only `/factorio/saves/_autosave1.zip` (`durable-dir.tar.zstd`) into the new Factorio container so Factorio migrates the save file in-place. All subsequent suspends and wakes return to fast (~1.5s) `SNAPSHOT_SCOPE_FULL` memory restores!

---

## Repository Layout

- [`cmd/factorio-gateway/main.go`](cmd/factorio-gateway/main.go) — Always-on UDP-to-WebSocket ingress gateway, `ResumeActor`/`SuspendActor` lifecycle manager, single-worker multiplexer, idle timer, and proactive/reactive `ActorTemplate` auto-upgrader.
- [`cmd/factorio-sidecar/main.go`](cmd/factorio-sidecar/main.go) — In-actor sidecar that bridges `/ws` frames to `127.0.0.1:34197/UDP`, probes Factorio readiness via UDP `ConnectionRequest`, and triggers RCON `/server-save` + `/version` barrier on `/prepare-suspend`.
- [`cmd/factorio-probe/main.go`](cmd/factorio-probe/main.go) — CLI probe that sends real Factorio 2.x UDP `ConnectionRequest` packets (`0x42 0x02 0x01 0x18...`) to test wake-on-UDP and measure resume latency without needing a graphical Factorio client.
- [`internal/tunnelproto/frame.go`](internal/tunnelproto/frame.go) — Wire framing (`[2B addrLen][addr][payload]`), Factorio UDP header validation, and client/server version extraction.
- [`internal/rcon/rcon.go`](internal/rcon/rcon.go) — Minimal Source RCON TCP client for talking to Factorio on `127.0.0.1:27015`.
- [`manifests/`](manifests/) — Kubernetes (`Namespace`, `WorkerPool`, `Deployment`, `Service`) and Agent Substrate (`ActorTemplate`) manifest templates.
- [`hack/deploy.sh`](hack/deploy.sh) — Builds images with `ko`, applies manifests, dynamically resolves the latest `factoriotools/factorio:stable` digest, registers `factorio-server`, and creates `world-vanilla` and `world-deathworld`.
- [`hack/test-e2e.sh`](hack/test-e2e.sh) — Automated end-to-end verification script testing wake-on-UDP, RCON status, multi-world multiplexing on the single worker pod, and idle auto-suspend.
- [`hack/prove-e2e.sh`](hack/prove-e2e.sh) — Full stateful proof that spawns a `steel-chest` with `42` `automation-science-pack`s at `(0.5, 0.5)` on `nauvis` via RCON Lua, multiplexes to `world-deathworld` (evicting `world-vanilla` to GCS), wakes `world-vanilla` back up from GCS, and verifies the chest and all 42 science packs survived.
- [`hack/watch-actors.sh`](hack/watch-actors.sh) — Live terminal dashboard (`make watch`) and transition logger (`make watch-transitions`) showing Actor states, WorkerPool pod assignments, GCS snapshot URIs, and gateway UDP counters.
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

### 4. Verify & Prove End-to-End (Headless Probe, Live Watch, & Stateful World Persistence)
You can watch Actor state transitions live in one terminal while running the end-to-end stateful proof in another:

```bash
# Terminal 1: Live Actor & UDP Gateway dashboard (or `make watch-transitions` for timestamped log)
make watch

# Terminal 2: Full stateful proof (spawns a steel-chest with 42 science packs at (0.5, 0.5) on nauvis,
# multiplexes to world-deathworld evicting world-vanilla to GCS, wakes world-vanilla back up from GCS,
# and verifies the chest + 42 science packs survived!)
make prove

# Trigger an immediate Docker Hub check & ActorTemplate upgrade check on factorio-gateway:
make check-update
```

To also verify the automatic idle-timeout RCON save + suspend cycle using a shortened 30-second timeout (and automatically restore the 10-minute timeout afterward):

```bash
make verify-with-idle
```

### 5. Connect with a Real Factorio Client
Find the external LoadBalancer IP:

```bash
kubectl -n factorio get svc factorio-gateway
```

In the Factorio 2.1.x game client (**Multiplayer -> Connect to address**):
- **Vanilla World:** `<EXTERNAL-IP>:34197` (walk to `(0.5, 0.5)` at spawn to see the `steel-chest` containing `42` `automation-science-pack`s!)
- **Deathworld:** `<EXTERNAL-IP>:34198`

*(Note: If the world is currently suspended, the first connection attempt triggers `ResumeActor` in the background; if a cold restore or version migration takes longer than the client's retry window, simply click **Connect** a second time once the world is awake.)*
