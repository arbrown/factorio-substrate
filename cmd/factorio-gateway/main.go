// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command factorio-gateway is an always-on lightweight UDP gateway pod that:
//  1. Captures inbound Factorio UDP traffic on one or more ports (e.g. 34197
//     for world-vanilla, 34198 for world-deathworld).
//  2. Filters out 1-packet internet UDP scanner noise.
//  3. Wakes the target Factorio Actor on demand via atenet-router (and if
//     another idle Factorio world is occupying the single WorkerPool pod,
//     cleanly saves and suspends it first).
//  4. Tunnels UDP datagrams over WebSocket (with automatic rotation before
//     Envoy's 5-minute route timeout) to factorio-sidecar inside the Actor.
//  5. Automatically saves (via RCON /prepare-suspend) and calls SuspendActor
//     after 10 minutes (configurable via --idle-timeout) of zero bidirectional
//     UDP traffic.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/arbrown/factorio-substrate/internal/tunnelproto"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const targetActorHeader = "ate-target-actor"

// ActorSuspender abstracts the Substrate Control Plane SuspendActor RPC so the
// gateway can be unit-tested without a live ate-api-server.
type ActorSuspender interface {
	SuspendActor(ctx context.Context, atespace, name string) error
}

type grpcActorSuspender struct {
	client ateapipb.ControlClient
}

func (g *grpcActorSuspender) SuspendActor(ctx context.Context, atespace, name string) error {
	_, err := g.client.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     name,
		},
	})
	return err
}

func newGRPCActorSuspender(address, caFile, clientCertBundle, serverName string) (ActorSuspender, func(), error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read ateapi CA file %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("no certificates found in %q", caFile)
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: serverName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			pemBytes, err := os.ReadFile(clientCertBundle)
			if err != nil {
				return nil, err
			}
			cert, err := tls.X509KeyPair(pemBytes, pemBytes)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, nil, fmt.Errorf("dial ateapi %q: %w", address, err)
	}
	return &grpcActorSuspender{client: ateapipb.NewControlClient(conn)}, func() { _ = conn.Close() }, nil
}

type WorldState string

const (
	WorldStateSuspended  WorldState = "SUSPENDED"
	WorldStateResuming   WorldState = "RESUMING"
	WorldStateRunning    WorldState = "RUNNING"
	WorldStateSuspending WorldState = "SUSPENDING"
)

type RouteSpec struct {
	UDPPort  int
	Atespace string
	Actor    string
}

func (r RouteSpec) ActorRef() string {
	return r.Atespace + "/" + r.Actor
}

type GatewayConfig struct {
	UDPBindIP          string
	HTTPListenAddr     string
	RouterHTTPAddr     string // e.g. "atenet-router.ate-system.svc.cluster.local:80"
	Routes             []RouteSpec
	IdleTimeout        time.Duration
	WSRotateInterval   time.Duration
	PreemptIdleGrace   time.Duration
	MaxActiveWorlds    int
	RequireFactorioPkt bool
}

type WorldTunnel struct {
	gw   *Gateway
	spec RouteSpec
	conn *net.UDPConn

	mu                sync.Mutex
	state             WorldState
	ws                *websocket.Conn
	wsCancel          context.CancelFunc
	pendingByClient   map[string][]byte
	clientAddrs       map[string]*net.UDPAddr
	burstTracker      map[string]time.Time
	lastInbound       time.Time
	lastOutboundReply time.Time
	lastStateChange   time.Time
	lastResumeLatency time.Duration

	packetsRx      atomic.Uint64
	packetsTx      atomic.Uint64
	scannerDropped atomic.Uint64
	wakeCount      atomic.Uint64
	suspendCount   atomic.Uint64
}

type Gateway struct {
	cfg       GatewayConfig
	suspender ActorSuspender
	worlds    map[string]*WorldTunnel // keyed by ActorRef ("atespace/actor")
	byPort    map[int]*WorldTunnel
	preemptMu sync.Mutex
}

func NewGateway(cfg GatewayConfig, suspender ActorSuspender) *Gateway {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	if cfg.WSRotateInterval <= 0 {
		cfg.WSRotateInterval = 4 * time.Minute
	}
	if cfg.PreemptIdleGrace <= 0 {
		cfg.PreemptIdleGrace = 5 * time.Second
	}
	if cfg.MaxActiveWorlds <= 0 {
		cfg.MaxActiveWorlds = 1
	}
	gw := &Gateway{
		cfg:       cfg,
		suspender: suspender,
		worlds:    make(map[string]*WorldTunnel, len(cfg.Routes)),
		byPort:    make(map[int]*WorldTunnel, len(cfg.Routes)),
	}
	for _, r := range cfg.Routes {
		wt := &WorldTunnel{
			gw:              gw,
			spec:            r,
			state:           WorldStateSuspended,
			pendingByClient: make(map[string][]byte),
			clientAddrs:     make(map[string]*net.UDPAddr),
			burstTracker:    make(map[string]time.Time),
			lastStateChange: time.Now(),
		}
		gw.worlds[r.ActorRef()] = wt
		gw.byPort[r.UDPPort] = wt
	}
	return gw
}

func (gw *Gateway) Start(ctx context.Context) error {
	for _, wt := range gw.worlds {
		addr := &net.UDPAddr{
			IP:   net.ParseIP(gw.cfg.UDPBindIP),
			Port: wt.spec.UDPPort,
		}
		conn, err := net.ListenUDP("udp4", addr)
		if err != nil {
			return fmt.Errorf("listen UDP %s for %s: %w", addr, wt.spec.ActorRef(), err)
		}
		wt.conn = conn
		slog.Info("Listening for Factorio UDP traffic",
			"udp_addr", conn.LocalAddr().String(),
			"actor", wt.spec.ActorRef(),
			"idle_timeout", gw.cfg.IdleTimeout,
		)
		go wt.udpReadLoop(ctx)
	}
	go gw.idleReaperLoop(ctx)
	return nil
}

func (gw *Gateway) Close() {
	for _, wt := range gw.worlds {
		if wt.conn != nil {
			_ = wt.conn.Close()
		}
		wt.mu.Lock()
		if wt.wsCancel != nil {
			wt.wsCancel()
		}
		if wt.ws != nil {
			_ = wt.ws.Close()
		}
		wt.mu.Unlock()
	}
}

func (wt *WorldTunnel) LocalUDPAddr() net.Addr {
	return wt.conn.LocalAddr()
}

func (wt *WorldTunnel) udpReadLoop(ctx context.Context) {
	buf := make([]byte, 65535)
	for {
		n, raddr, err := wt.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		pkt := append([]byte(nil), buf[:n]...)
		wt.handleInboundUDP(ctx, raddr, pkt)
	}
}

func (wt *WorldTunnel) handleInboundUDP(ctx context.Context, raddr *net.UDPAddr, pkt []byte) {
	clientKey := raddr.String()
	now := time.Now()

	wt.mu.Lock()
	wt.clientAddrs[clientKey] = raddr
	state := wt.state

	if state == WorldStateSuspended || state == WorldStateSuspending {
		if !wt.shouldWakeLocked(clientKey, pkt, now) {
			wt.scannerDropped.Add(1)
			wt.mu.Unlock()
			return
		}
		wt.packetsRx.Add(1)
		wt.lastInbound = now
		wt.pendingByClient[clientKey] = pkt
		if state == WorldStateSuspended {
			wt.state = WorldStateResuming
			wt.lastStateChange = now
			wt.wakeCount.Add(1)
			slog.Info("UDP traffic waking Factorio Actor",
				"actor", wt.spec.ActorRef(),
				"client", clientKey,
				"port", wt.spec.UDPPort,
			)
			go wt.activateWorld(ctx)
		}
		wt.mu.Unlock()
		return
	}

	wt.packetsRx.Add(1)
	wt.lastInbound = now

	if state == WorldStateResuming || wt.ws == nil {
		// Buffer latest handshake packet per client while ResumeActor is in flight.
		wt.pendingByClient[clientKey] = pkt
		wt.mu.Unlock()
		return
	}

	ws := wt.ws
	frame, err := tunnelproto.EncodeFrame(clientKey, pkt)
	if err != nil {
		wt.mu.Unlock()
		return
	}
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	writeErr := ws.WriteMessage(websocket.BinaryMessage, frame)
	wt.mu.Unlock()

	if writeErr != nil {
		slog.Warn("WebSocket write failed; buffering packet for reconnect", "actor", wt.spec.ActorRef(), "err", writeErr)
		wt.mu.Lock()
		wt.pendingByClient[clientKey] = pkt
		wt.mu.Unlock()
	}
}

// shouldWakeLocked determines whether an inbound UDP packet to a SUSPENDED
// world should wake the Actor. It accepts either a recognizable Factorio
// client packet (e.g. ConnectionRequest 0x02) or a burst of >=2 packets from
// the same IP within 5 seconds, filtering out 1-packet internet scanners.
func (wt *WorldTunnel) shouldWakeLocked(clientKey string, pkt []byte, now time.Time) bool {
	if tunnelproto.IsLikelyFactorioClientPacket(pkt) {
		return true
	}
	if wt.gw.cfg.RequireFactorioPkt {
		return false
	}
	host, _, err := net.SplitHostPort(clientKey)
	if err != nil {
		host = clientKey
	}
	if prev, ok := wt.burstTracker[host]; ok && now.Sub(prev) <= 5*time.Second {
		delete(wt.burstTracker, host)
		return true
	}
	wt.burstTracker[host] = now
	return false
}

func (wt *WorldTunnel) activateWorld(ctx context.Context) {
	// If the WorkerPool is full (e.g. 1 replica) and another world is currently
	// running or resuming without active player traffic, suspend it first so
	// this world can claim the worker pod.
	wt.gw.preemptIdleWorldsFor(ctx, wt)

	tunnelCtx, cancel := context.WithCancel(ctx)
	wt.mu.Lock()
	wt.wsCancel = cancel
	wt.mu.Unlock()

	go wt.maintainWebSocketLoop(tunnelCtx)
}

func (gw *Gateway) preemptIdleWorldsFor(ctx context.Context, target *WorldTunnel) {
	gw.preemptMu.Lock()
	defer gw.preemptMu.Unlock()

	active := make([]*WorldTunnel, 0, len(gw.worlds))
	for _, other := range gw.worlds {
		if other == target {
			continue
		}
		other.mu.Lock()
		st := other.state
		other.mu.Unlock()
		if st == WorldStateRunning || st == WorldStateResuming {
			active = append(active, other)
		}
	}

	if len(active) < gw.cfg.MaxActiveWorlds {
		return
	}

	// Find a world that has had no outbound Factorio UDP replies within PreemptIdleGrace
	// (or the least-recently active world) and suspend it to free a worker pod.
	for _, candidate := range active {
		candidate.mu.Lock()
		idleFor := time.Since(candidate.lastOutboundReply)
		if candidate.lastOutboundReply.IsZero() {
			idleFor = time.Since(candidate.lastStateChange)
		}
		candidate.mu.Unlock()

		if idleFor >= gw.cfg.PreemptIdleGrace {
			slog.Info("Multiplexing WorkerPool: suspending idle world to make room for incoming world",
				"suspending_actor", candidate.spec.ActorRef(),
				"waking_actor", target.spec.ActorRef(),
				"idle_for", idleFor.Round(time.Millisecond),
			)
			_ = candidate.suspendNow(ctx, "preempted-for-"+target.spec.Actor)
			return
		}
	}
}

func (wt *WorldTunnel) maintainWebSocketLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		startDial := time.Now()
		ws, err := wt.dialActorWebSocket(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("Dialing Actor WebSocket via atenet-router failed; retrying",
				"actor", wt.spec.ActorRef(),
				"err", err,
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		now := time.Now()
		wt.mu.Lock()
		wasResuming := wt.state == WorldStateResuming
		if wasResuming {
			wt.lastResumeLatency = now.Sub(startDial)
			wt.state = WorldStateRunning
			wt.lastStateChange = now
			if wt.lastOutboundReply.IsZero() {
				wt.lastOutboundReply = now
			}
			slog.Info("Factorio Actor resumed and WebSocket tunnel established",
				"actor", wt.spec.ActorRef(),
				"resume_latency", wt.lastResumeLatency.Round(time.Millisecond),
			)
		}
		wt.ws = ws

		// Flush any buffered client handshake packets immediately.
		for clientKey, pkt := range wt.pendingByClient {
			if frame, err := tunnelproto.EncodeFrame(clientKey, pkt); err == nil {
				_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_ = ws.WriteMessage(websocket.BinaryMessage, frame)
			}
			delete(wt.pendingByClient, clientKey)
		}
		wt.mu.Unlock()

		// Read replies until WS rotation timer fires or connection closes.
		wt.wsReadLoop(ctx, ws)

		wt.mu.Lock()
		if wt.ws == ws {
			wt.ws = nil
		}
		st := wt.state
		wt.mu.Unlock()
		_ = ws.Close()

		if st != WorldStateRunning && st != WorldStateResuming {
			return
		}
	}
}

func (wt *WorldTunnel) dialActorWebSocket(ctx context.Context) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	u := url.URL{
		Scheme: "ws",
		Host:   wt.gw.cfg.RouterHTTPAddr,
		Path:   "/ws",
	}
	headers := http.Header{}
	headers.Set(targetActorHeader, wt.spec.ActorRef())

	dialer := websocket.Dialer{
		HandshakeTimeout: 90 * time.Second,
	}
	ws, resp, err := dialer.DialContext(dialCtx, u.String(), headers)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("dial %s (status %d: %s): %w", u.String(), resp.StatusCode, strings.TrimSpace(string(body)), err)
		}
		return nil, err
	}
	return ws, nil
}

func (wt *WorldTunnel) wsReadLoop(ctx context.Context, ws *websocket.Conn) {
	rotateTimer := time.NewTimer(wt.gw.cfg.WSRotateInterval)
	defer rotateTimer.Stop()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-rotateTimer.C:
			slog.Info("Rotating WebSocket tunnel before Envoy route-timeout", "actor", wt.spec.ActorRef())
			_ = ws.Close()
		case <-done:
		}
	}()
	defer close(done)

	for {
		msgType, frame, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		clientKey, payload, err := tunnelproto.DecodeFrame(frame)
		if err != nil {
			continue
		}

		wt.mu.Lock()
		raddr := wt.clientAddrs[clientKey]
		// Reset the 10-minute idle timer ONLY when Factorio sends a reply packet!
		wt.lastOutboundReply = time.Now()
		wt.mu.Unlock()

		if raddr == nil {
			parsed, err := net.ResolveUDPAddr("udp4", clientKey)
			if err != nil {
				continue
			}
			raddr = parsed
		}
		if _, err := wt.conn.WriteToUDP(payload, raddr); err == nil {
			wt.packetsTx.Add(1)
		}
	}
}

func (gw *Gateway) idleReaperLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, wt := range gw.worlds {
				wt.mu.Lock()
				if wt.state != WorldStateRunning {
					wt.mu.Unlock()
					continue
				}
				lastActivity := wt.lastOutboundReply
				if lastActivity.IsZero() {
					lastActivity = wt.lastStateChange
				}
				idleDuration := now.Sub(lastActivity)
				wt.mu.Unlock()

				if idleDuration >= gw.cfg.IdleTimeout {
					slog.Info("No Factorio UDP traffic for idle timeout; suspending Actor",
						"actor", wt.spec.ActorRef(),
						"idle_duration", idleDuration.Round(time.Second),
					)
					_ = wt.suspendNow(ctx, "idle-timeout")
				}
			}
		}
	}
}

func (wt *WorldTunnel) suspendNow(ctx context.Context, reason string) error {
	wt.mu.Lock()
	if wt.state == WorldStateSuspended || wt.state == WorldStateSuspending {
		wt.mu.Unlock()
		return nil
	}
	wt.state = WorldStateSuspending
	wt.lastStateChange = time.Now()
	cancel := wt.wsCancel
	wt.wsCancel = nil
	ws := wt.ws
	wt.ws = nil
	wt.mu.Unlock()

	// 1. Trigger RCON /server-save on the Actor's sidecar before closing and suspending.
	wt.prepareActorSuspend(ctx)

	// 2. Close active WebSocket tunnel so atunnel has zero active requests during SuspendActor.
	if cancel != nil {
		cancel()
	}
	if ws != nil {
		_ = ws.Close()
	}

	// 3. Call SuspendActor on ate-api-server.
	var suspendErr error
	if wt.gw.suspender != nil {
		suspendCtx, suspendCancel := context.WithTimeout(ctx, 60*time.Second)
		defer suspendCancel()
		suspendErr = wt.gw.suspender.SuspendActor(suspendCtx, wt.spec.Atespace, wt.spec.Actor)
		if suspendErr != nil {
			slog.Error("SuspendActor RPC failed", "actor", wt.spec.ActorRef(), "reason", reason, "err", suspendErr)
		} else {
			slog.Info("Factorio Actor suspended to GCS", "actor", wt.spec.ActorRef(), "reason", reason)
		}
	}

	wt.mu.Lock()
	wt.state = WorldStateSuspended
	wt.lastStateChange = time.Now()
	wt.suspendCount.Add(1)
	wt.mu.Unlock()
	return suspendErr
}

func (wt *WorldTunnel) prepareActorSuspend(ctx context.Context) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	u := fmt.Sprintf("http://%s/prepare-suspend", wt.gw.cfg.RouterHTTPAddr)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, nil)
	if err != nil {
		return
	}
	req.Header.Set(targetActorHeader, wt.spec.ActorRef())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("prepare-suspend call failed", "actor", wt.spec.ActorRef(), "err", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	slog.Info("Actor prepared for suspend (RCON save)", "actor", wt.spec.ActorRef(), "response", strings.TrimSpace(string(body)))
}

func (gw *Gateway) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/status", gw.handleStatus)
	mux.HandleFunc("/world-status", gw.handleWorldStatus)
	mux.HandleFunc("/suspend", gw.handleManualSuspend)
	return mux
}

func (gw *Gateway) handleStatus(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	worlds := make([]map[string]any, 0, len(gw.cfg.Routes))
	for _, r := range gw.cfg.Routes {
		wt := gw.worlds[r.ActorRef()]
		wt.mu.Lock()
		st := wt.state
		lastReply := wt.lastOutboundReply
		lastChange := wt.lastStateChange
		resumeMs := wt.lastResumeLatency.Milliseconds()
		clients := len(wt.clientAddrs)
		wt.mu.Unlock()

		var idleSec float64
		var remainingSec float64
		if st == WorldStateRunning {
			ref := lastReply
			if ref.IsZero() {
				ref = lastChange
			}
			idle := now.Sub(ref)
			idleSec = idle.Seconds()
			rem := gw.cfg.IdleTimeout - idle
			if rem > 0 {
				remainingSec = rem.Seconds()
			}
		}

		worlds = append(worlds, map[string]any{
			"udp_port":               r.UDPPort,
			"actor":                  r.ActorRef(),
			"state":                  st,
			"known_clients":          clients,
			"packets_rx":             wt.packetsRx.Load(),
			"packets_tx":             wt.packetsTx.Load(),
			"scanner_dropped":        wt.scannerDropped.Load(),
			"wake_count":             wt.wakeCount.Load(),
			"suspend_count":          wt.suspendCount.Load(),
			"last_resume_latency_ms": resumeMs,
			"idle_seconds":           int(idleSec),
			"idle_remaining_seconds": int(remainingSec),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"idle_timeout":      gw.cfg.IdleTimeout.String(),
		"max_active_worlds": gw.cfg.MaxActiveWorlds,
		"worlds":            worlds,
	})
}

func (gw *Gateway) handleWorldStatus(w http.ResponseWriter, r *http.Request) {
	actorRef := r.URL.Query().Get("actor")
	if actorRef == "" && len(gw.cfg.Routes) > 0 {
		actorRef = gw.cfg.Routes[0].ActorRef()
	}
	cmd := r.URL.Query().Get("cmd")
	targetURL := fmt.Sprintf("http://%s/status", gw.cfg.RouterHTTPAddr)
	if cmd != "" {
		targetURL += "?cmd=" + url.QueryEscape(cmd)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set(targetActorHeader, actorRef)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (gw *Gateway) handleManualSuspend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	actorRef := r.URL.Query().Get("actor")
	wt, ok := gw.worlds[actorRef]
	if !ok {
		http.Error(w, "unknown actor "+actorRef, http.StatusNotFound)
		return
	}
	if err := wt.suspendNow(r.Context(), "manual-api"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"suspended": actorRef})
}

func parseRoutes(raw string) ([]RouteSpec, error) {
	var routes []RouteSpec
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid route %q (want <udpPort>=<atespace>/<actor>)", entry)
		}
		port, err := strconv.Atoi(parts[0])
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid UDP port in route %q", entry)
		}
		actorParts := strings.SplitN(parts[1], "/", 2)
		if len(actorParts) != 2 || actorParts[0] == "" || actorParts[1] == "" {
			return nil, fmt.Errorf("invalid actor ref in route %q (want <atespace>/<actor>)", entry)
		}
		routes = append(routes, RouteSpec{
			UDPPort:  port,
			Atespace: actorParts[0],
			Actor:    actorParts[1],
		})
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("at least one route is required")
	}
	return routes, nil
}

func main() {
	var (
		routesRaw        string
		udpBindIP        string
		httpListenAddr   string
		routerHTTPAddr   string
		idleTimeout      time.Duration
		wsRotateInterval time.Duration
		preemptIdleGrace time.Duration
		maxActiveWorlds  int
		ateapiAddr       string
		ateapiCAFile     string
		ateapiClientCert string
		ateapiServerName string
	)
	flag.StringVar(&routesRaw, "routes", "34197=factorio/world-vanilla,34198=factorio/world-deathworld", "Comma-separated <udpPort>=<atespace>/<actor> routes")
	flag.StringVar(&udpBindIP, "udp-bind-ip", "0.0.0.0", "IP address to bind UDP listeners on")
	flag.StringVar(&httpListenAddr, "http-listen-addr", ":8080", "HTTP status/health listen address")
	flag.StringVar(&routerHTTPAddr, "router-http-addr", "atenet-router.ate-system.svc.cluster.local:80", "Substrate atenet-router HTTP address")
	flag.DurationVar(&idleTimeout, "idle-timeout", 10*time.Minute, "Duration of zero Factorio UDP traffic before suspending the Actor")
	flag.DurationVar(&wsRotateInterval, "ws-rotate-interval", 4*time.Minute, "WebSocket rotation interval (must be < Envoy 5m route-timeout)")
	flag.DurationVar(&preemptIdleGrace, "preempt-idle-grace", 5*time.Second, "Minimum idle time before a world can be preempted when multiplexing a full WorkerPool")
	flag.IntVar(&maxActiveWorlds, "max-active-worlds", 1, "Maximum concurrent running worlds (matches WorkerPool replicas)")
	flag.StringVar(&ateapiAddr, "ateapi-address", "api.ate-system.svc.cluster.local:443", "Substrate Control Plane gRPC address")
	flag.StringVar(&ateapiCAFile, "ateapi-ca-file", "/run/servicedns-ca/trust-bundle.pem", "CA bundle for verifying ate-api-server")
	flag.StringVar(&ateapiClientCert, "ateapi-client-cert", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "Pod Identity client cert bundle for mTLS to ate-api-server")
	flag.StringVar(&ateapiServerName, "ateapi-server-name", "api.ate-system.svc", "TLS ServerName for ate-api-server")
	flag.Parse()

	routes, err := parseRoutes(routesRaw)
	if err != nil {
		slog.Error("Failed to parse --routes", "err", err)
		os.Exit(1)
	}

	suspender, closeSuspender, err := newGRPCActorSuspender(ateapiAddr, ateapiCAFile, ateapiClientCert, ateapiServerName)
	if err != nil {
		slog.Error("Failed to initialize ateapi gRPC client", "err", err)
		os.Exit(1)
	}
	defer closeSuspender()

	gw := NewGateway(GatewayConfig{
		UDPBindIP:          udpBindIP,
		HTTPListenAddr:     httpListenAddr,
		RouterHTTPAddr:     routerHTTPAddr,
		Routes:             routes,
		IdleTimeout:        idleTimeout,
		WSRotateInterval:   wsRotateInterval,
		PreemptIdleGrace:   preemptIdleGrace,
		MaxActiveWorlds:    maxActiveWorlds,
		RequireFactorioPkt: false,
	}, suspender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := gw.Start(ctx); err != nil {
		slog.Error("Failed to start gateway UDP listeners", "err", err)
		os.Exit(1)
	}
	defer gw.Close()

	slog.Info("Starting factorio-gateway HTTP status server", "listen", httpListenAddr)
	if err := http.ListenAndServe(httpListenAddr, gw.HTTPHandler()); err != nil {
		slog.Error("factorio-gateway HTTP server exited", "err", err)
		os.Exit(1)
	}
}
