# Factorio on Agent Substrate (`factorio-ate`)

Running stateful, UDP-based dedicated game servers (`factoriotools/factorio:stable`) as on-demand, auto-suspending **Agent Substrate (`ate.dev/v1`) Actors** on GKE, with **multi-world multiplexing** over a single warm `WorkerPool` pod.

This repository is the companion implementation to the follow-up post on [Factorio on GKE with Spot VMs and Auto-Shutdown](https://wasd.drewbrown.me/posts/factorio-gke-spot/).

See [`TRADEOFFS.md`](TRADEOFFS.md) for a detailed breakdown of the 11 architectural challenges ("holes poked") and how this implementation resolves them without modifying Agent Substrate.

---

## Architecture Overview

```
Factorio Client (UDP :34197 Vanilla / :34198 Deathworld / ...)
       │
       ▼  (GCP VPC Firewall Rule udp:34197-34297 -> Node hostPort)
┌──────────────────────────────────────────────────────────┐
│ factorio-gateway Pod (Spot control-pool, always-on)     │
│  1. Listens on dynamic hostPorts for arbitrary maps      │
│  2. Drops small/malformed internet UDP scanner packets   │
│  3. If target world is SUSPENDED:                        │
│     • Preempts & saves any other idle world on the pool  │
│     • Wakes Actor via atenet-router (mTLS WSS :443/ws)   │
│     • Buffers initial UDP handshake datagrams            │
│  4. Opens WebSocket tunnel via atenet-router (:443/ws)   │
│     with header `ate-target-actor: factorio/<world>`     │
│  5. Tracks bidirectional UDP traffic; after 10m of       │
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
- **1 warm `WorkerPool` pod** (`factorio`, `replicas: 1`)
- **4 distinct Factorio `Actor` worlds**, each generating its own custom map preset, super-dense resource settings (`map-gen-settings.json`), server name/description, charted map tags, and starter kit chest on first connect:
  1. `factorio/world-vanilla` (`:34197/UDP`) — **`[Substrate #1] Super-Dense Megabase (600% Rich Ores)`**: `--preset rich-resources` with 600% richness (`richness: 6, size: 4, frequency: 2`), peaceful mode, huge starting area (`3x`), zero cliffs, and a starter chest with Power Armor, Exoskeletons, Roboports, and Express Belts.
  2. `factorio/world-deathworld` (`:34198/UDP`) — **`[Substrate #2] Deathworld Biter Swarm (400% Enemy Bases)`**: `--preset death-world` with 400% biter nest frequency & size (`frequency: 4, size: 4, richness: 4`), small starting area (`0.75x`), 400% rich ores, and a 4-turret armed perimeter bunker at spawn stocked with Piercing Ammo and Stone Walls.
  3. `factorio/world-railworld` (`:34199/UDP`) — **`[Substrate #3] Rail World Express (Giant Outpost Patches)`**: `--preset rail-world` with colossal 600% size & richness distant ore patches (`frequency: 0.25, size: 6, richness: 6`), biter expansion disabled, zero cliffs, and a starter depot with 1,000 Rails, 5 Locomotives, 10 Cargo Wagons, and Rocket Fuel.
  4. `factorio/world-ribbonworld` (`:34200/UDP`) — **`[Substrate #4] Ribbon World 64-Tile Strip (600% Rich)`**: `--preset ribbon-world` with `height: 64` (a narrow 64-tile tall horizontal strip where the black map void is visible at the top and bottom of your screen!), 600% rich ores, and a starter chest with 500 Landfill and Fast Belts.

Connecting to `:34197` wakes `world-vanilla` into the warm worker pod. Connecting to `:34198`, `:34199`, or `:34200` while another world is idle automatically triggers an RCON `/server-save` on the active world, snapshots its memory + `/factorio` directory to GCS (`gs://<BUCKET_NAME>`), and restores the requested world into the exact same worker pod in ~1.4 seconds!

### Scale-to-Zero Architecture ("Option A" with Spot VMs & hostPort)
For personal/home use, keeping worker nodes running 24/7 or paying for cloud LoadBalancers is wasteful. We implement **Option A Scale-to-Zero with Spot VMs and hostPort ingress**, minimizing cluster footprint and operational overhead:
1. **Always-On Spot Control Plane (`control-pool`):** 1x `e2-medium` Spot node (30GB pd-standard). Runs the Substrate control plane (`ate-api-server`, `ate-controller`, `atenet-router`, `atenet-egress`, `podcertificate-controller`), `factorio-gateway`, and `postgres-0` (right-sized from 500Gi to 10Gi).
2. **On-Demand Spot Worker Node Pool (`substrate-worker-pool`):** `0 -> 1` autoscaling `n2-standard-2` Spot node (Intel Ice Lake, 50GB pd-balanced, tainted `ate.dev/sandboxClass=gvisor:NoSchedule`). Scales down to 0 nodes when idle and provisions on demand.
3. **Direct `hostPort` Ingress (Arbitrary Maps):** Replaces the GKE `LoadBalancer` Service with direct node `hostPort` bindings on `factorio-gateway` (`strategy: Recreate`) coupled with a GCP VPC firewall rule (`factorio-ate-allow-udp-hostport`) allowing `udp:34197-34297` (101 ports). Adding any number of maps to `WORLDS` in `hack/deploy.sh` automatically binds and exposes their ports without hitting LoadBalancer forwarding rule limits.
4. **Gateway Ingress Cold Wake:** On the first inbound UDP packet, `factorio-gateway` scales `WorkerPool/factorio` `0 -> 1`. GKE Cluster Autoscaler provisions the node in ~60–90 seconds while the gateway handles connection retries.
5. **Automatic Scale-to-Zero:** When all worlds are suspended for `--workerpool-idle-timeout` (default `2m`), `factorio-gateway` scales `WorkerPool/factorio` `1 -> 0`. GKE Cluster Autoscaler (`--autoscaling-profile=optimize-utilization`) then shuts down the worker node ~2 minutes later.
6. **Run Cost Optimization:**
   ```bash
   make optimize-cluster   # Runs ./hack/setup-cost-optimized-pools.sh
   ```

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
# Build and deploy Agent Substrate 1.0 control plane + atelet DaemonSet
source "$(pwd)/../factorio-substrate/.ate-dev-env.sh"
go run ./cmd/ate-setup deploy ate-system --no-dev-env --sandbox-classes=gvisor
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
Find the gateway node's external IP (`hostPort`):

```bash
GW_NODE="$(kubectl get pod -n factorio -l app=factorio-gateway -o jsonpath='{.items[0].spec.nodeName}')"
kubectl get node "${GW_NODE}" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}'
```

In the Factorio 2.1.x game client (**Multiplayer -> Connect to address**):
- **World #1 — Super-Dense Megabase (600% Rich Ores, Peaceful, No Cliffs):** `<EXTERNAL-IP>:34197`
- **World #2 — Deathworld Biter Swarm (400% Enemy Bases, Armed Spawn Bunker):** `<EXTERNAL-IP>:34198`
- **World #3 — Rail World Express (Giant 600% Outpost Patches, Train Depot Chest):** `<EXTERNAL-IP>:34199`
- **World #4 — Ribbon World 64-Tile Strip (64-Tile Tall Horizontal World, 600% Ores):** `<EXTERNAL-IP>:34200`

To reset all 4 worlds so they generate brand-new custom maps on next connect:
```bash
RESET_WORLDS=true ./hack/deploy.sh
```

*(Note: If the world is currently suspended, the first connection attempt triggers `ResumeActor` in the background; if a cold restore or version migration takes longer than the client's retry window, simply click **Connect** a second time once the world is awake.)*
