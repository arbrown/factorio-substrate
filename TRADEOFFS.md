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
  - Rather than forking Substrate's dataplane, we deploy an always-on **`factorio-gateway`** Pod (exposed via a `LoadBalancer` Service on UDP ports `34197`, `34198`, ...) and add a lightweight **`factorio-sidecar`** container alongside `factoriotools/factorio:stable` inside the `ActorTemplate`.
  - The Gateway encapsulates raw player UDP datagrams (preserving client `IP:port`) into WebSocket binary frames and sends them through `atenet-router` with header `ate-target-actor: <atespace>/<actor>`.
  - Inside the Actor sandbox, `factorio-sidecar` receives the WebSocket frames on TCP `:80` and forwards them as real UDP packets over loopback (`127.0.0.1:34197`) to the Factorio server, maintaining a dedicated local ephemeral UDP socket per player `IP:port`. Because loopback traffic never leaves the Actor's network namespace, it bypasses `ateom0` and `nftables` completely.
* **Side Effect (Server Browser Visibility):**
  - Because `actorNonDNSUDPDropRule` drops outbound UDP to non-DNS ports, Factorio's UDP heartbeats to `pingpong*.factorio.com` cannot egress. We set `"visibility": {"public": false, "lan": false}` in `server-settings.json` and connect directly via the Gateway's external `LoadBalancer` IP:port.

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
  - The original blog post used **GKE Autopilot + Spot VMs** and scaled the Deployment to `0` when idle, costing $0/hr in compute while off (at the cost of a 2–4 minute cold start when scaling back up).
  - Agent Substrate requires **GKE Standard** (for `atelet` privileged host operations, `gVisor` `runsc` management, `nftables`/netns setup, and K8s `podcertificaterequests` / `clustertrustbundles` APIs) and expressly warns against Spot VMs on `WorkerPool` nodes because an evicted worker pod that isn't suspended within 30 minutes puts the Actor into terminal `ACTOR_STATE_CRASHED`.
  - Furthermore, a warm `WorkerPool` with `replicas: 1` still keeps 1 physical worker pod running in the cluster even when all Actors are `SUSPENDED`.
* **Why Substrate Makes Sense Here:**
  1. **Instant On-Demand Activation:** Instead of waiting minutes for GKE Autopilot to provision a Spot VM and cold-boot Factorio, players connect directly from the Factorio game client and resume from GCS in seconds.
  2. **N-to-1 World Multiplexing:** A single `replicas: 1` `WorkerPool` pod can host an arbitrary number of Factorio worlds (Vanilla, Space Age, Deathworld, Modded) plus other Substrate actors (like AI coding agents or Jupyter notebooks). Only the currently active world occupies compute; all idle worlds live purely as GCS objects (`gs://.../snapshots/...`) with zero Persistent Disks (`PVC`s) attached.

### Trade-off 7: ConfigMaps & InitContainers in `ActorTemplate`
* **The Constraint:**
  - Unlike Kubernetes `Deployment`s, Substrate's `ActorTemplate` does not yet support `initContainers` or `ConfigMap` volume mounts (only `durableDir`, `externalVolumeTemplate`, and `systemInfo`).
* **Our Solution:**
  - `factorio-sidecar` mounts the shared `/factorio` `durableDir` volume and writes `/factorio/config/*.json` (`server-settings.json`, `map-gen-settings.json`, `map-settings.json`, `server-adminlist.json`) and `/factorio/mods/mod-list.json` on startup before Factorio initializes, supporting world presets via flags/env vars.
