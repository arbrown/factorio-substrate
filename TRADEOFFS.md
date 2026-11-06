# Factorio on Agent Substrate: Architecture & Trade-offs Record

This document records the architectural design, technical hurdles, and trade-offs discovered while adapting [Factorio on GKE with Spot VMs and Auto-Shutdown](https://wasd.drewbrown.me/posts/factorio-gke-spot/) to run on **Agent Substrate**.

---

## 1. Context & Goal

In the original setup, a Factorio headless server (`factoriotools/factorio:stable`) ran on a GKE Autopilot cluster using Spot VMs, a `10Gi` `ReadWriteOnce` `PersistentVolumeClaim` (`factorio-data-pvc`), and Kubernetes `CronJob`s to scale the `Deployment` up at 5 PM and down to `0` at midnight.

The "Wishlist" at the end of that post was:
> *"If I could add one feature, it would be the on-demand scaling up and down as we connect and disconnect from the server."*

Using **Agent Substrate**, we turn Factorio worlds into stateful **Substrate Actors** that:
1. **Wake on demand** when a player connects over UDP (restoring from a snapshot in Google Cloud Storage).
2. **Auto-suspend** after 10 minutes of no active player UDP traffic (saving RAM and `/factorio` disk state back to GCS).
3. **Multiplex multiple Factorio worlds** (e.g., Vanilla on UDP `:34197` and Deathworld/Space-Age on UDP `:34198`) onto a **single warm `WorkerPool` pod (`replicas: 1`)**, swapping worlds in and out of the worker pod on demand.

---

## 2. Explicit Technical Trade-offs & "Holes" Poked in the Design

### Trade-off 1: Substrate Networking is HTTP-Only and Drops Non-DNS UDP
* **The Constraint:**
  - Substrate places every active Actor inside an isolated interior network namespace (`169.254.17.2/30`) connected to the Worker Pod via a `veth` pair (`ateom0`).
  - The Substrate ingress router (`atenet-router` running Envoy + `ext_proc`) and the worker-side tunnel (`atunnel`) only route HTTP/1.1, HTTP/2, gRPC, and WebSocket upgrades over TCP.
  - Furthermore, `ateomnet` (`InstallActorNftablesRules` / `actorNonDNSUDPDropRule`) explicitly installs an `nftables` forward-chain rule on the worker pod that **drops all UDP packets originating from the Actor where `dport != 53` (DNS)**.
* **Our Solution (Unmodified Substrate):**
  - Rather than forking Substrate's dataplane, we deploy an always-on **`factorio-gateway`** Pod on `control-pool` (exposed via direct node **`hostPort`s** on UDP ports `34197`, `34198`, ... with a GCP VPC firewall rule for `udp:34197-34297`) and add a lightweight **`factorio-sidecar`** container alongside `factoriotools/factorio:stable` inside the `ActorTemplate`.
  - The Gateway encapsulates raw player UDP datagrams (preserving client `IP:port`) into WebSocket binary frames and sends them through `atenet-router` with header `ate-target-actor: <atespace>/<actor>`.
  - Inside the Actor sandbox, `factorio-sidecar` receives the WebSocket frames on TCP `:80` and forwards them as real UDP packets over loopback (`127.0.0.1:34197`) to the Factorio server, maintaining a dedicated local ephemeral UDP socket per player `IP:port`. Because loopback traffic never leaves the Actor's network namespace, it bypasses `ateom0` and `nftables` completely.
* **Side Effect (Server Browser Visibility):**
  - Because `actorNonDNSUDPDropRule` drops outbound UDP to non-DNS ports, Factorio's UDP heartbeats to `pingpong*.factorio.com` cannot egress. We set `"visibility": {"public": false, "lan": false}` in `server-settings.json` and connect directly via the Gateway node's external `hostPort` IP:port.

### Trade-off 2: Envoy's 5-Minute Route Timeout (`--route-timeout`)
* **The Constraint:**
  - By default, `atenet-router` configures Envoy with a 5-minute (`300s`) route timeout on workload requests. A single long-lived WebSocket connection carrying encapsulated UDP traffic would be severed by Envoy every 5 minutes during gameplay.
* **Our Solution:**
  - Because UDP is connectionless and `factorio-sidecar` maintains the per-client loopback UDP socket map in memory across WebSocket reconnections, `factorio-gateway` automatically rotates/reconnects the WebSocket stream transparently (and immediately reconnects on EOF) without dropping the player's Factorio UDP session.

### Trade-off 3: Internet UDP Scanners vs. Wake-on-UDP and Idle Timeout
* **The Constraint:**
  - Any public cloud `LoadBalancer` listening on UDP is constantly probed by internet port scanners (Shodan, Censys, etc.) sending single stray UDP packets.
  - If *any* inbound UDP packet triggered `ResumeActor` or reset the 10-minute idle timer, background internet noise would wake the Factorio Actor constantly and prevent it from ever suspending.
* **Our Solution:**
  - **Wake filter:** To wake a suspended Actor, `factorio-gateway` requires a burst of $\ge 2$ UDP packets from the same source IP within a short window (or a valid Factorio UDP packet structure), which a real Factorio client naturally sends during its connection handshake, ignoring 1-packet internet scans.
  - **Idle timer filter:** Once an Actor is `RUNNING`, the 10-minute idle timer is **only reset when Factorio sends a UDP reply packet back to a client** (bidirectional traffic). Stray scanner packets are ignored by Factorio, elicit no reply, and never reset the idle timer. Meanwhile, even a single connected AFK player exchanges UDP packets with Factorio at 60 Hz, keeping the server alive until the last player disconnects.

### Trade-off 4: `FULL` Snapshot Restore Latency vs. Factorio's Connection Timeout
* **The Constraint:**
  - When a player clicks "Connect", the Factorio client retries its UDP handshake for ~10–15 seconds before timing out.
  - Restoring a `SNAPSHOT_CONTENT_SCOPE_FULL` snapshot (which includes the gVisor memory image of Factorio plus `/factorio` save files in `durableDir`) from GCS takes a few seconds. If the Gateway drops the client's initial UDP packets while waiting for `atenet-router` / `ate-api-server` to finish `ResumeActor`, those early handshake attempts are lost.
* **Our Solution:**
  - While an Actor is transitioning from `SUSPENDED` to `RUNNING`, `factorio-gateway` buffers the most recent UDP packets per client and flushes them down the WebSocket tunnel the instant the Actor's tunnel becomes active.

### Trade-off 5: Full RAM Snapshot (`SNAPSHOT_CONTENT_SCOPE_FULL`) vs. Game Server State
* **The Constraint:**
  - Checkpointing a live C++ game server (`SNAPSHOT_CONTENT_SCOPE_FULL`) freezes its exact RAM state, open sockets, and internal timers, and restores them hours or days later.
* **Our Solution:**
  - Factorio's headless server defaults to `"auto_pause": true` when zero players are connected. Because `factorio-gateway` waits for 10 minutes of zero player traffic after the last player disconnects before suspending, Factorio has already finalized the player's disconnect, auto-saved, and entered its clean idle auto-pause state before gVisor checkpoints its memory.
  - As an additional safeguard (and to support `SNAPSHOT_CONTENT_SCOPE_DATA` fallback if desired), `factorio-sidecar` also configures Factorio's local RCON port and issues a `/save` command via `/prepare-suspend` right before `factorio-gateway` calls `SuspendActor` (including when preempting an idle world to multiplex another world onto the worker).

### Trade-off 6: Cost & Infrastructure Reality (GKE Autopilot + Spot vs. GKE Standard + Warm WorkerPool)
* **The Constraint:**
  - The original blog post used **GKE Autopilot + Spot VMs** and scaled the Deployment to `0` when idle, incurring no compute cost while off (at the cost of a 2–4 minute cold start when scaling back up).
  - Agent Substrate requires **GKE Standard** (for `atelet` privileged host operations, `gVisor` `runsc` management, `nftables`/netns setup, and K8s `podcertificaterequests` / `clustertrustbundles` APIs) and expressly warns against Spot VMs on `WorkerPool` nodes because an evicted worker pod that isn't suspended within 30 minutes puts the Actor into terminal `ACTOR_STATE_CRASHED`.
  - Furthermore, a warm `WorkerPool` with `replicas: 1` still keeps 1 physical worker pod running in the cluster even when all Actors are `SUSPENDED`.
* **Why Substrate Makes Sense Here:**
  1. **Instant On-Demand Activation:** Instead of waiting minutes for GKE Autopilot to provision a Spot VM and cold-boot Factorio, players connect directly from the Factorio game client and resume from GCS in seconds.
  2. **N-to-1 World Multiplexing:** A single `replicas: 1` `WorkerPool` pod can host an arbitrary number of Factorio worlds (Vanilla, Space Age, Deathworld, Modded) plus other Substrate actors (like AI coding agents or Jupyter notebooks). Only the currently active world occupies compute; all idle worlds live purely as GCS objects (`gs://.../snapshots/...`) with zero Persistent Disks (`PVC`s) attached.

### Trade-off 7: ConfigMaps, InitContainers, & gVisor `FileAccess: exclusive` on `durableDir`
* **The Constraint:**
  - Unlike Kubernetes `Deployment`s, Substrate's `ActorTemplate` does not yet support `initContainers` or `ConfigMap` volume mounts (only `durableDir`, `externalVolumeTemplate`, and `systemInfo`).
  - Furthermore, inside gVisor (`runsc`), each container in the Actor sandbox runs its own `runsc-gofer` process with `FileAccess: exclusive` enabled by default on bind mounts. If two containers (`sidecar` and `factorio`) both mount the same `durableDir` (`/factorio`) concurrently, gVisor's per-container dentry cache becomes incoherent across the two gofer mounts.
* **Our Solution:**
  - Only the `factorio` container mounts the `/factorio` `durableDir` volume (preserving gVisor's `FileAccess: exclusive` invariant), and its startup script seeds `/factorio/config/*.json` from `/opt/factorio/data/server-settings.example.json` before invoking `/opt/factorio/bin/x64/factorio`. Meanwhile, `factorio-sidecar` interacts with Factorio purely over actor-local loopback sockets (`127.0.0.1:34197/UDP` and `127.0.0.1:27015/TCP`).

### Trade-off 8: Container Linux Capabilities vs. Upstream `/docker-entrypoint.sh`
* **The Constraint:**
  - Agent Substrate containers inside the gVisor sandbox default to only `AUDIT_WRITE`, `KILL`, and `NET_BIND_SERVICE` capabilities (`CAP_SETUID`, `CAP_SETGID`, and `CAP_CHOWN` are not granted by default).
  - When `factoriotools/factorio:stable` starts as UID 0, its upstream `/docker-entrypoint.sh` attempts to run `usermod`, `groupmod`, `chown -R factorio:factorio /factorio`, and `runuser -u factorio`, which fail without those capabilities.
* **Our Solution:**
  - In [`manifests/factorio-template.yaml.tmpl`](manifests/factorio-template.yaml.tmpl), we override `command` on the `factorio` container to initialize `/factorio/config/*`, run `/opt/factorio/bin/x64/factorio --create /factorio/saves/_autosave1.zip` if no save exists yet, and `exec /opt/factorio/bin/x64/factorio` directly with `-c /factorio/config/config.ini`. This avoids capability errors while keeping all `/factorio` volume access inside the `factorio` container.

### Trade-off 9: `atelet` OCI Image Unpacker vs. `ko` Default Base Image
* **The Constraint:**
  - When `ko` builds Go binaries without a `.ko.yaml`, it defaults to `cgr.dev/chainguard/static:latest`. That image's rootfs layer includes a `/dev/console` character device tar entry (`tar.TypeChar`, `typeflag "3"`), which `atelet`'s `imageCache.EnsureImage` rejects with `unhandled tar entry typeflag "3"`.
* **Our Solution:**
  - We configure [`.ko.yaml`](.ko.yaml) with `defaultBaseImage: gcr.io/distroless/static-debian13:latest`, matching Agent Substrate's own `.ko.yaml` and ensuring all image layers contain only regular files, directories, and symlinks.

### Trade-off 10: RCON `/server-save` Flush Timing & `.tmp.zip` Cleanup on Cold Boot
* **The Constraint:**
  - When `factorio-sidecar` issues `/server-save` over RCON right before `SuspendActor`, Factorio writes the save to `/factorio/saves/_autosave1.tmp.zip` and atomically renames it to `/factorio/saves/_autosave1.zip`. If `SuspendActor` freezes the container while `_autosave1.tmp.zip` is still being written—and the Actor later cold-boots from `SNAPSHOT_CONTENT_SCOPE_DATA` (`--start-server-load-latest`)—Factorio selects `_autosave1.tmp.zip` as the newest `.zip` file in `/factorio/saves/` and fails with an I/O error on the incomplete archive.
* **Our Solution:**
  - In `factorio-sidecar`'s `/prepare-suspend` handler, we issue a follow-up synchronous RCON `/version` query after `/server-save` as a serialization barrier on Factorio's main loop, and in `factorio-template.yaml.tmpl` we run `rm -f /factorio/saves/*.tmp.zip` before `--start-server-load-latest` so any interrupted temporary archive is discarded in favor of the last complete `.zip` save.

### Trade-off 11: Steam Auto-Updates vs. Immutable Golden Memory Snapshots (Proactive + Reactive Zero-Touch Upgrades)
* **The Constraint:**
  - Steam automatically updates players' desktop Factorio clients whenever Wube releases a new `stable` patch (for example, `2.1.23` $\rightarrow$ `2.1.24`), and Factorio's multiplayer protocol strictly requires exact `major.minor.patch` version equality between client and server.
  - However, in Agent Substrate, `ActorTemplate`s are immutable and pin container images by `@sha256:...` digest, and a `SNAPSHOT_CONTENT_SCOPE_FULL` gVisor memory checkpoint cannot be restored on top of a different container rootfs/binary.
* **Our Solution (Substrate `TemplateReplaced` + Dual Auto-Upgrader in `factorio-gateway`):**
  - **In-Place Actor Migration via `SNAPSHOT_SCOPE_DATA`:** Agent Substrate tracks the `ActorTemplate` UID each Actor's snapshot was taken on in `.status.currentActorTemplateUid`. When `factorio/factorio-server` is replaced with a new `@sha256:...` digest, it gets a new `metadata.uid` and builds a new Golden Snapshot. The next time an existing Actor (`world-vanilla` or `world-deathworld`) wakes up, Substrate's `loadActorForResume` detects `builtOnTemplateUID != actorTemplate.GetMetadata().GetUid()` (`TemplateReplaced = true`) and automatically switches that single restore to `SNAPSHOT_SCOPE_DATA` (`RESUME_SOURCE_COLD_BOOT`). `ateworker-gvisor` extracts only the Actor's `durable-dir.tar.zstd` (`/factorio/saves/_autosave1.zip`) into the **new** Factorio container, cold-boots the new Factorio binary to migrate the save in-place, and stamps the new `currentActorTemplateUid`. Every subsequent suspend/resume returns to fast (~1.5s) `SNAPSHOT_SCOPE_FULL` memory restores!
  - **1. Proactive Background Docker Hub Watcher (`--image-poll-interval=30m`):** While all worlds are `SUSPENDED`, `factorio-gateway` periodically queries Docker Hub (`factoriotools/factorio:stable`). When the upstream `@sha256:...` digest changes, `factorio-gateway` automatically replaces `factorio/factorio-server` over mTLS gRPC (`GetActorTemplate` $\rightarrow$ `DeleteActorTemplate` $\rightarrow$ `CreateActorTemplate`) so the new Golden Snapshot is pre-built before players even launch the game.
  - **2. Reactive Client Version Sniffer on UDP `ConnectionRequest`:** If Steam updates a player's client *before* the 30-minute background poll fires, `factorio-gateway` inspects the 4-byte `ApplicationVersion` (`[2, minor, patch, 0]`) inside the client's first UDP `ConnectionRequest` (`0x02`) packet. Seeing `client_version > known_server_version`, it immediately queries Docker Hub and upgrades `factorio/factorio-server` on the spot.

### Trade-off 12: Per-Actor Custom Map Presets vs. Shared Golden Snapshot & gVisor `system-info` VFS Dentry Caching
* **The Constraint:**
  - All 4 Factorio worlds (`world-vanilla`, `world-deathworld`, `world-railworld`, `world-ribbonworld`) share a single `ActorTemplate` (`factorio/factorio-server`) and its pre-warmed Golden Snapshot (`SNAPSHOT_CONTENT_SCOPE_FULL`).
  - If `--create /factorio/saves/_autosave1.zip` only runs during the Golden Snapshot build in `ate-golden`, then every Actor restored from the Golden Snapshot inherits the exact same map seed, preset, and server name.
  - Furthermore, because gVisor (`runsc`) mounts `system-info` (`/run/ate`) with `FileAccess: exclusive`, if any process inside the sandbox opens or reads `/run/ate/actor-name` *during* the Golden Snapshot build (`ate-golden`), gVisor caches the Golden Snapshot's dentry/inode (`<golden-template-uid>`) in the kernel VFS page cache, causing reads after `RestoreWorkload` (`SNAPSHOT_SCOPE_FULL`) to return the stale cached golden UID instead of the restored Actor's name.
* **Our Solution (Deferred `system-info` Read + Loopback Control Channel + UDP Handshake Coalescing):**
  1. **Zero `/run/ate` Reads During `ate-golden`:** `factorio-sidecar` maintains an `activated` atomic flag that remains `false` during `ate-golden` (when `ateworker-gvisor` only polls `GET /readyz`) and flips to `true` only on the first real Actor request (`/ws`, `/status`, or `/prepare-suspend`) after `RestoreWorkload`. Because `/run/ate/actor-name` is never opened during `ate-golden`, its first open after `RestoreWorkload` is guaranteed to fetch the live Actor name (`world-vanilla`, `world-deathworld`, `world-railworld`, or `world-ribbonworld`) fresh from the host gofer.
  2. **Bash `/dev/tcp` Loopback Control Channel (`127.0.0.1:27016`):** Because `factoriotools/factorio:stable` does not include `curl` or `wget` and only the `factorio` container mounts `/factorio` (`durableDir`), `factorio-sidecar` exposes a lightweight line-based TCP control listener on `127.0.0.1:27016` that the `factorio` container queries using built-in `/bin/bash` `/dev/tcp/127.0.0.1/27016`.
  3. **First-Connect Custom World Generation & UDP Coalescing:** When an Actor wakes from the Golden Snapshot on its very first connect, `factorio-sidecar` sets `activated = true` and coalesces incoming player `ConnectionRequest` retry packets per `clientAddr` in memory while the `factorio` container stops the placeholder golden server, fetches the Actor-specific `server-settings.json`, `map-gen-settings.json`, and `--preset` from `127.0.0.1:27016`, runs `--create /factorio/saves/_autosave1.zip --preset <preset> --map-gen-settings /factorio/config/map-gen-settings.json`, and boots Factorio on the new map. Once `factorio-sidecar` seeds the new world's map tags and starter chest via RCON (`initWorldViaRCON`), it flushes the freshest `ConnectionRequest` packet to `127.0.0.1:34197`. On all subsequent connects after that world is suspended to GCS, `SAVED_ACTOR == TARGET_ACTOR`, so the world resumes in ~1.4s with zero map-generation overhead.

### Trade-off 13: Scale-to-Zero Worker Nodes ("Option A" with Spot VMs) vs. Cold Boot Friction
* **The Constraint:**
  - Running a warm worker node (`c3-standard-4` or `n2-standard-2`) 24/7 consumes significant baseline compute resources even when no one is playing.
  - Factorio's game client times out its initial connection handshake after ~15–20 seconds if no reply is received.
  - Provisioning a cold GKE worker node, pulling DaemonSet images, starting `atelet`, and registering the `WorkerPool` pod takes ~60–90 seconds.
* **Our Solution (Option A Architecture):**
  - **Two-Tier Spot Node Pool Design:**
    1. **Always-On Spot Control Plane (`control-pool`):** Single `e2-medium` Spot node (30GB pd-standard). Hosts `factorio-gateway`, `postgres-0` (right-sized from 500Gi to 10Gi), `ate-api-server`, `ate-controller`, `atenet-router`, `atenet-egress`, and `podcertificate-controller`.
    2. **On-Demand Spot Worker Pool (`substrate-worker-pool`):** `0 -> 1` autoscaling `n2-standard-2` Spot node with `--min-cpu-platform="Intel Ice Lake"` (ensuring CPUID consistency across gVisor checkpoints), tainted with `ate.dev/sandboxClass=gvisor:NoSchedule`. Scaled to 0 nodes when idle and provisions on demand.
  - **Gateway Ingress Cold Wake:**
    - On the very first inbound UDP `ConnectionRequest`, `factorio-gateway` calls the in-cluster Kubernetes API to scale `WorkerPool/factorio` `0 -> 1`.
    - GKE Cluster Autoscaler (`--autoscaling-profile=optimize-utilization`) spins up the worker node in ~60–90 seconds.
    - While dialing `atenet-router`, the gateway loops with exponential backoff until the worker pod is registered and the WebSocket tunnel is established (up to `--resume-timeout=5m`).
    - Stale client packets buffered during the initial 15s are pruned to avoid Factorio sequence desync.
    - Once the WebSocket tunnel opens, `lastOutboundReply` is unconditionally refreshed, granting a full fresh idle timeout window.
  - **Automatic Scale-to-Zero Reaper:**
    - When all Factorio worlds have been suspended for `--workerpool-idle-timeout` (default `2m`), `factorio-gateway` patches `WorkerPool/factorio` back to `replicas: 0`.
    - Because the worker node is tainted `ate.dev/sandboxClass=gvisor:NoSchedule` and `WorkerPool` pods and `atelet` DaemonSet carry `safe-to-evict: "true"`, GKE Cluster Autoscaler detects zero non-DaemonSet pods and safely shuts down the worker VM ~2 minutes later.
  - **The User Friction Trade-off:**
    - When cold: The user launches Factorio, connects to `:34197`, waits ~60–90 seconds for the node to provision (or triggers `curl -X POST http://<gateway-ip>:8080/scale?replicas=1` / launches a browser probe), and joins.
    - Once warm: Multiplexing between all 4 worlds takes only ~1.4 seconds.
    - When idle: Everything scales to zero automatically with zero manual teardown needed.

### Trade-off 14: Direct `hostPort` Ingress & VPC Firewall Range vs. Cloud LoadBalancer Costs
* **The Constraint:**
  - A standard cloud LoadBalancer has per-forwarding-rule limits and charges per rule/port.
  - Exposing many Factorio worlds (e.g. 10 or 20 maps) would require either multiple LoadBalancer forwarding rules or port range hacks that cloud load balancers do not support smoothly for UDP.
* **Our Solution:**
  - We remove `Service/factorio-gateway` (`type: LoadBalancer`) entirely and configure `Deployment/factorio-gateway` with direct **`hostPort`s** pinned to `control-pool` via `nodeSelector: cloud.google.com/gke-nodepool: control-pool`.
  - To prevent port collision on rollouts on the single control node, the deployment uses `strategy: {type: Recreate}` so the old pod terminates and frees the host ports before the new pod binds them.
  - We create a single GCP VPC Firewall rule (`factorio-ate-allow-udp-hostport`) allowing port range `udp:34197-34297` (101 ports) directly to nodes.
  - In `hack/deploy.sh`, adding any number of maps to `WORLDS` dynamically templates their sequential `containerPort` and `hostPort` entries into the pod specification without LoadBalancer rule limits.


