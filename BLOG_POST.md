+++
title = "The Factory Sleeps Until You Connect: On-Demand Factorio Worlds with Agent Substrate on GKE"
date = 2026-10-26T10:00:00-06:00
draft = true
description = "Cracking the wishlist item from my original GKE Factorio guide: true wake-on-UDP, automatic idle suspend to GCS, and multi-world multiplexing on a single warm worker pod using Agent Substrate and gVisor snapshots."
images = ["/images/factorio-gke-spot/factoriok8s.png"]
categories = ["Projects", "Kubernetes"]
tags = ["factorio", "gaming", "gke", "gcp", "kubernetes", "agent-substrate", "gvisor", "tutorial"]
+++

At the very end of my [previous Factorio on GKE post](/posts/factorio-gke-spot/), I left a little wishlist item:

> *If I could add one feature, it would be the on-demand scaling up and down as we connect and disconnect from the server. I haven't cracked that bit yet, and I figure that the single-digit dollars it would save me are probably not worth spending too much time thinking about. But if you've got a solution, I'd love to hear it!*

Well... it still might only save single-digit dollars, but I've been spending a lot of time working with **[Agent Substrate](https://github.com/agent-substrate/substrate)** on [GKE](https://cloud.google.com/kubernetes-engine?utm_campaign=CDR_0x145aeba1_default_b423920039&utm_medium=external&utm_source=blog) lately (like in my [Agent Storybook Project](/posts/substrate-asp/)). One afternoon it hit me: *Wait a second. Agent Substrate is built to freeze stateful gVisor sandboxes into Google Cloud Storage (GCS) when they're idle, and wake them back up on demand when traffic arrives. Could a Factorio server be an Actor?*

Obviously, Agent Substrate was designed for AI agents, code interpreters, and RL training environments—not C++ headless game servers. Which makes hosting Factorio on it an *awesome* tech demo.

Even better: once your Factorio server is an Actor whose memory and save files live in GCS when suspended, you don't just get wake-on-connect and auto-shutdown after 10 minutes of silence. You also get **multi-world multiplexing**. You can host a Vanilla world on port `34197`, a Space Age Deathworld on port `34198`, and a heavily modded world on port `34199`—all sharing a **single warm worker pod** (`replicas: 1`), with zero PersistentVolumeClaims (`PVC`s) attached while idle!

Let's walk through how this works, the wonderfully gnarly networking and gVisor holes we had to poke (and solve!) to make a stateful UDP game server run on an HTTP-native agent platform, and how you can deploy it yourself.

---

## The Concept (and the Holes We Had to Poke)

In Theory Land, the architecture sounds almost too simple:
1. Keep a tiny, always-on **Gateway Pod** listening on a UDP `LoadBalancer` (`:34197`).
2. When a player clicks **Connect** in Factorio, the Gateway catches the first UDP packet, calls `ResumeActor` on Agent Substrate, and forwards the UDP packets to the Factorio Actor.
3. When nobody has sent or received a UDP packet for 10 minutes, the Gateway calls `SuspendActor`, freezing Factorio back to GCS.

When you actually look under the hood of Agent Substrate and Factorio's multiplayer protocol, **nine very real engineering problems** jump out at you. Here is what happens when Theory Land meets reality—and how we solved each one without modifying a single line of Agent Substrate.

```
Factorio Client (UDP :34197 Vanilla / :34198 Deathworld)
       │
       ▼  (GKE LoadBalancer Service, UDP 34197 & 34198)
┌──────────────────────────────────────────────────────────┐
│ factorio-gateway Pod (factorio namespace, always-on)     │
│  1. Drops small/malformed internet UDP scanner packets   │
│  2. If target world is SUSPENDED:                        │
│     • Preempts & saves any other idle world on the pool  │
│     • Calls ateapi.Actors/ResumeActor (mTLS gRPC)        │
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

### Hole #1: Agent Substrate Drops Non-DNS UDP (By Design!)
Agent Substrate routes all Actor ingress through `atenet-router` (an Envoy + HTTP/gRPC ext_proc proxy that inspects the `ate-target-actor` HTTP header and tunnels traffic over mTLS HTTP/2 CONNECT to the worker node). Even more importantly, inside the worker pod (`ateomnet`), Substrate installs an explicit `nftables` firewall rule (`actorNonDNSUDPDropRule`) on the Actor's virtual interface (`169.254.17.2`) that **drops all UDP packets except DNS (`dport 53`)**.

* **The Fix (WebSocket-to-Loopback Sidecar):** An Actor's `ActorTemplate` can run multiple containers sharing the same gVisor network namespace (`127.0.0.1`). Loopback traffic never touches `169.254.17.2` or the `nftables` drop rule! So we pair the unmodified `factoriotools/factorio:stable` container with a lightweight Go sidecar (`factorio-sidecar`) listening on `:80/ws`. The Gateway upgrades an HTTP connection through `atenet-router` to a WebSocket (using `ate-target-actor: factorio/world-vanilla`) and streams raw UDP datagrams over the WebSocket. The sidecar unpacks them and forwards them over `127.0.0.1:34197/UDP` to Factorio.
* We also set `"visibility": {"public": false, "lan": false}` in `server-settings.json` so Factorio never tries to send outbound UDP heartbeats to `pingpong*.factorio.com`.

### Hole #2: Multi-Player Source `IP:port` Identity (No Shared Socket!)
Factorio's multiplayer server identifies connected peers purely by their UDP source `IP:port`. If the sidecar forwarded every player's packets through a single local UDP socket, Factorio would see all players coming from the same `127.0.0.1:<ephemeral>` port and corrupt their sessions the moment Player 2 joined.

* **The Fix:** Every WebSocket binary frame between `factorio-gateway` and `factorio-sidecar` carries a tiny header: `[2-byte big-endian addrLen][client "IP:port" string][raw UDP payload]`. For each unique client `IP:port`, the sidecar allocates a dedicated ephemeral local UDP socket (`127.0.0.1:<port_i>`) connected to `127.0.0.1:34197`. Factorio sees each player as a distinct local UDP peer, and replies on `<port_i>` map 1-to-1 back to the right remote player!

### Hole #3: Envoy's 5-Minute Route Timeout vs. Multi-Hour Factory Sessions
`atenet-router` configures Envoy with `--route-timeout=5m` (300 seconds) for HTTP and WebSocket streams. A Factorio session lasts hours. If the WebSocket closes at the 5-minute mark without coordination, you'd get a nasty lag spike.

* **The Fix:** Individual Factorio UDP datagrams are independent! Our `factorio-gateway` proactively opens a fresh WebSocket tunnel every **4 minutes** (`--ws-rotate-interval=4m`) and atomically swaps the active pointer before Envoy's 5-minute timer ever fires. Inside `factorio-sidecar`, the per-client ephemeral UDP sockets (`127.0.0.1:<port_i>`) stay open across WebSocket rotations, so Factorio never notices a thing.

### Hole #4: Internet UDP Scanners (The "Shodan Keeps My Factory Awake" Problem)
If you expose UDP `:34197` on a public IP, internet scanners (Shodan, Censys, random noise) will hit it with stray packets every few minutes. If *any* UDP packet woke the Actor and reset the 10-minute idle timer, your server would never sleep.

* **The Fix (Two-Stage Validation):**
  1. **Wake filter:** `factorio-gateway` drops tiny/malformed scanner probes at the door (`< 8` bytes) before considering `ResumeActor`.
  2. **Bidirectional idle timer:** Once the Actor is running, inbound UDP packets alone **do not** reset the 10-minute idle timer after the initial 60-second grace window. Only **valid Factorio UDP replies coming back from the Factorio server** (`packets_out`) reset the idle timer. Random internet noise gets ignored by Factorio, elicits zero reply packets, and never delays auto-suspend!

### Hole #5: gVisor `FileAccess: exclusive`, Capabilities, and Base Images
Running an off-the-shelf game server container inside a snapshottable gVisor sandbox surfaced three classic systems gotchas:
1. **`FileAccess: exclusive` on `durableDir`:** In gVisor (`runsc`), each container in an Actor runs its own `runsc-gofer` with `FileAccess: exclusive` enabled by default on bind mounts. If two containers (`sidecar` and `factorio`) both mount `/factorio` concurrently, their dentry caches collide. Moving all `/factorio` file initialization into the `factorio` container's startup script so *only* `factorio` mounts the `durableDir` keeps `FileAccess: exclusive` fast and happy.
2. **Default Linux capabilities vs. `/docker-entrypoint.sh`:** Substrate containers default to `AUDIT_WRITE`, `KILL`, and `NET_BIND_SERVICE` (no `CAP_SETUID`/`CAP_CHOWN`). The upstream `/docker-entrypoint.sh` in `factoriotools/factorio` tries to run `usermod`, `chown`, and `runuser` when started as root. Bypassing `/docker-entrypoint.sh` and `exec`-ing `/opt/factorio/bin/x64/factorio` directly solves this cleanly.
3. **`ko` base image vs. `atelet` unpacker:** `ko`'s default `cgr.dev/chainguard/static:latest` base image includes a `/dev/console` character device tar entry (`typeflag "3"`), which `atelet`'s OCI layer unpacker rejects. Pointing `.ko.yaml` at `gcr.io/distroless/static-debian13:latest` fixes it immediately.

---

## Multi-World Multiplexing on a Single Worker Pod

In my original GKE Autopilot + Spot setup, scaling down to `0` replicas meant $0/hr in compute while off, at the cost of waiting 2–4 minutes for GKE Autopilot to provision a node whenever we wanted to play (or relying on a rigid `5 PM to Midnight` CronJob).

Agent Substrate makes a different trade-off: you run a warm `WorkerPool` pod (`replicas: 1`) on GKE Standard so Actors can resume in seconds instead of minutes. At first glance, keeping 1 warm worker pod around for 1 Factorio server seems to defeat the purpose of suspending it.

**Unless you multiplex multiple worlds (and agents!) onto that single worker pod.**

In our setup (`manifests/factorio-substrate.yaml.tmpl`), we configure a single 1-replica `WorkerPool` (`2 vCPU, 4 GiB RAM`) and bind **two separate UDP ports** on the Gateway:
* **UDP `:34197` -> `factorio/world-vanilla`**
* **UDP `:34198` -> `factorio/world-deathworld`**

When `world-vanilla` is running on the single worker pod (with zero active players) and someone connects to `:34198` (`world-deathworld`), look at what `factorio-gateway` does automatically:
1. Calls `/prepare-suspend` on `world-vanilla`'s sidecar, which connects to Factorio's local RCON port (`127.0.0.1:27015`) and executes `/server-save`.
2. Calls `SuspendActor` on `factorio/world-vanilla`, checkpointing its gVisor memory and `/factorio` save directory to `gs://<BUCKET>/factorio/`.
3. Calls `ResumeActor` on `factorio/world-deathworld`, restoring it onto the **exact same worker pod** and flushing the buffered UDP handshake packets!

---

## Step-by-Step: Deploying Factorio on Agent Substrate

All of the code, manifests, and end-to-end test scripts are in the companion repository (`factorio-substrate`). Here are the key pieces and how to spin up your own `factorio-ate` cluster.

### 1. The `ActorTemplate` (`manifests/factorio-template.yaml.tmpl`)

Just like in the [Storybook post](/posts/substrate-asp/), the `ActorTemplate` tells Substrate how to build the Golden Snapshot and how to checkpoint our Actor (`SNAPSHOT_CONTENT_SCOPE_FULL` + a `durableDir` volume at `/factorio`).

Notice how `sidecar`'s `/readyz` probe actually sends a real Factorio 2.x UDP `ConnectionRequest` packet (`0x42 0x02 0x01 0x17...`) to `127.0.0.1:34197` and waits for a genuine `ConnectionRequestReply` (`0xd2...`) before telling Substrate that the Golden Snapshot is ready!

```yaml
metadata:
  atespace: factorio
  name: factorio-server
workerSelector:
  matchLabels:
    workload: factorio
containers:
- name: sidecar
  image: ko://github.com/arbrown/factorio-substrate/cmd/factorio-sidecar
  command:
  - /ko-app/factorio-sidecar
  args:
  - "--listen-addr=:80"
  - "--factorio-udp-addr=127.0.0.1:34197"
  - "--factorio-rcon-addr=127.0.0.1:27015"
  - "--factorio-dir="
  - "--rcon-password=substrate-factorio-rcon"
  - "--system-info-dir=/run/ate"
  readyz:
    httpGet:
      path: /readyz
      port: 80
    timeoutSeconds: 180
  volumeMounts:
  - name: system-info
    mountPath: /run/ate
- name: factorio
  image: "index.docker.io/factoriotools/factorio@sha256:74f41f9450f2ba8464c445fb712f5ca1643a6b92b495c7a1f301bb3f1c7e2940"
  command:
  - /bin/bash
  - -ec
  - |
    mkdir -p /factorio/saves /factorio/config /factorio/mods /factorio/scenarios /factorio/script-output
    printf "[path]\nread-data=__PATH__executable__/../../data\nwrite-data=/factorio\n" > /factorio/config/config.ini
    if [ ! -f /factorio/config/server-settings.json ]; then
      sed \
        -e 's/"public": true/"public": false/g' \
        -e 's/"lan": true/"lan": false/g' \
        -e 's/"require_user_verification": true/"require_user_verification": false/g' \
        /opt/factorio/data/server-settings.example.json > /factorio/config/server-settings.json
    fi
    if [ ! -f /factorio/config/server-adminlist.json ]; then
      printf '[\n  "drewbr"\n]\n' > /factorio/config/server-adminlist.json
    fi
    if [ -z "$(ls -A /factorio/saves/*.zip 2>/dev/null || true)" ]; then
      /opt/factorio/bin/x64/factorio \
        -c /factorio/config/config.ini \
        --create /factorio/saves/_autosave1.zip
    fi
    exec /opt/factorio/bin/x64/factorio \
      -c /factorio/config/config.ini \
      --port 34197 \
      --server-settings /factorio/config/server-settings.json \
      --server-adminlist /factorio/config/server-adminlist.json \
      --rcon-bind 127.0.0.1:27015 \
      --rcon-password "substrate-factorio-rcon" \
      --server-id /factorio/config/server-id.json \
      --mod-directory /factorio/mods \
      --start-server-load-latest
  readyz:
    httpGet:
      path: /readyz
      port: 80
    timeoutSeconds: 180
  volumeMounts:
  - name: factorio-data
    mountPath: /factorio
resources:
  limits:
  - name: cpu
    quantity: "2"
  - name: memory
    quantity: 4Gi
snapshotsConfig:
  onPause: SNAPSHOT_CONTENT_SCOPE_FULL
  onCommit: SNAPSHOT_CONTENT_SCOPE_FULL
  storageLocation: gs://${BUCKET_NAME}/factorio/
sandboxConfig:
  sandboxClass: SANDBOX_CLASS_GVISOR
  configName: gvisor-default
volumes:
- name: factorio-data
  durableDir: {}
- name: system-info
  systemInfo:
    dataSources:
    - actorMetadata:
        items:
        - field: ACTOR_METADATA_FIELD_NAME
          path: actor-name
        - field: ACTOR_METADATA_FIELD_ATESPACE
          path: atespace
```

### 2. Bootstrapping the Cluster and Agent Substrate

First, configure your environment variables in `.ate-dev-env.sh`:

```bash
export PROJECT_ID="your-gcp-project"
export CLUSTER_NAME="factorio-ate"
export BUCKET_NAME="${PROJECT_ID}-factorio-ate"
export CLUSTER_LOCATION="us-central1-a"
export KO_DOCKER_REPO="gcr.io/${PROJECT_ID}/factorio-ate-images"
```

From your Agent Substrate checkout, bootstrap the GKE cluster (`c3-standard-4` nodes) and GCS snapshot bucket, and install `ate-system`:

```bash
go run ./tools/setup-gcp bootstrap \
  --project "$PROJECT_ID" \
  --cluster "$CLUSTER_NAME" \
  --bucket "$BUCKET_NAME" \
  --zone "$CLUSTER_LOCATION"

gcloud container node-pools update substrate-node-pool \
  --cluster="$CLUSTER_NAME" \
  --zone="$CLUSTER_LOCATION" \
  --project="$PROJECT_ID" \
  --no-enable-autoupgrade

./hack/install-ate.sh --deploy-ate-system
```

### 3. Deploying the Gateway, WorkerPool, and Factorio Worlds

Now, from the `factorio-substrate` repo, run:

```bash
make deploy
```

Under the hood, `./hack/deploy.sh`:
1. Builds and pushes `factorio-gateway` and `factorio-sidecar` using `ko`.
2. Creates the `factorio` `WorkerPool` (`replicas: 1`) and `factorio-gateway` `Deployment` + `LoadBalancer` `Service` (exposing UDP `34197` and `34198`).
3. Creates the `factorio-server` `ActorTemplate` and waits for the Golden Snapshot (with a pre-generated Factorio 2.1.23 Space Age map) to be saved to GCS.
4. Creates two Actor instances—`factorio/world-vanilla` and `factorio/world-deathworld`—so both start in `ACTOR_STATE_SUSPENDED` in GCS.

### 4. Testing Wake-on-UDP, Multiplexing, and Auto-Suspend

You don't even need a gaming PC handy to test the UDP wake-up and multiplexing loop! We built a CLI tool (`cmd/factorio-probe`) that crafts a real Factorio 2.x UDP `ConnectionRequest` datagram and verifies the server's `ConnectionRequestReply`:

```bash
make verify-with-idle
```

Here's what happens during the automated end-to-end test:
1. Both `world-vanilla` and `world-deathworld` start in `ACTOR_STATE_SUSPENDED`.
2. `factorio-probe` sends a Factorio UDP handshake to `localhost:34197`.
3. `factorio-gateway` catches the UDP packet, calls `ResumeActor` on `factorio/world-vanilla`, opens the WebSocket tunnel through `atenet-router`, and flushes the handshake. Factorio replies with a valid `0xd2` `ConnectionRequestReply` (echoing Factorio version `2.1.23`), and `/world-status` queries live RCON `/version`, `/seed`, `/time`, and `/players`.
4. Next, `factorio-probe` sends a UDP handshake to `localhost:34198` (`world-deathworld`).
5. Seeing that the single worker pod is occupied by an idle `world-vanilla`, `factorio-gateway` triggers an RCON `/server-save` on `world-vanilla`, suspends `world-vanilla` back to GCS, and resumes `world-deathworld` into the **same worker pod**!
6. Finally, after the idle timeout elapses with zero player UDP traffic, `factorio-gateway` automatically runs `/server-save` and suspends `world-deathworld` back to `ACTOR_STATE_SUSPENDED`.

---

## Connecting from the Factorio Game Client

Grab the external IP of the UDP LoadBalancer:

```bash
kubectl -n factorio get svc factorio-gateway
```

In Factorio (**Multiplayer -> Connect to address**):
* **Vanilla World:** `<EXTERNAL-IP>:34197`
* **Deathworld:** `<EXTERNAL-IP>:34198`

*(Pro tip: Because `factorio-gateway` buffers your initial UDP packets while `ResumeActor` streams the memory snapshot from GCS, a warm restore often completes before the client gives up. If a cold restore or world swap takes longer than Factorio's ~10s client retry window, the first connection attempt wakes the world in the background and your second click connects immediately!)*

The factory can finally sleep when we're not playing—and wake itself up the moment we come back. 🚀
