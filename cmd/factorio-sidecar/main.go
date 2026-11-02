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

// Command factorio-sidecar runs inside the Substrate Actor sandbox alongside
// the unmodified factoriotools/factorio container. It:
//  1. Serves per-Actor server-settings.json, map-gen-settings.json, and map
//     preset over a local TCP control channel (127.0.0.1:27016) so each Actor
//     generates its own distinct Factorio world on first connect.
//  2. Exposes /readyz on port 80, probing Factorio's UDP port (127.0.0.1:34197)
//     so Substrate only takes the Golden Snapshot once Factorio is ready.
//  3. Exposes /ws on port 80 to bridge WebSocket-encapsulated UDP datagrams
//     from factorio-gateway to 127.0.0.1:34197 over actor-local loopback.
//  4. Exposes /prepare-suspend and /status on port 80, using local RCON
//     (127.0.0.1:27015) to trigger clean saves and report live world metadata.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arbrown/factorio-substrate/internal/rcon"
	"github.com/arbrown/factorio-substrate/internal/tunnelproto"
	"github.com/gorilla/websocket"
)

type Config struct {
	ListenAddr              string
	ControlAddr             string
	FactorioUDPAddr         string
	FactorioRCONAddr        string
	FactorioDir             string
	SystemInfoDir           string
	RCONPassword            string
	RequireUserVerification bool
	SessionIdleTimeout      time.Duration
}

type clientSession struct {
	clientAddr string
	conn       *net.UDPConn
	lastActive atomic.Int64
}

type SidecarServer struct {
	cfg      Config
	upgrader websocket.Upgrader

	mu       sync.RWMutex
	sessions map[string]*clientSession

	wsMu     sync.Mutex
	activeWS *websocket.Conn

	// activated is false during ate-golden Golden Snapshot creation so
	// /run/ate/actor-name is never opened/cached in gVisor's VFS before
	// CheckpointWorkload. It becomes true on the first /ws, /status, or
	// /prepare-suspend request after RestoreWorkload.
	activated     atomic.Bool
	controlActive atomic.Bool

	worldMu    sync.RWMutex
	readyActor string

	packetsIn  atomic.Uint64
	packetsOut atomic.Uint64
}

func NewSidecarServer(cfg Config) *SidecarServer {
	if cfg.SessionIdleTimeout <= 0 {
		cfg.SessionIdleTimeout = 2 * time.Minute
	}
	s := &SidecarServer{
		cfg:      cfg,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		sessions: make(map[string]*clientSession),
	}
	return s
}

// StartControlListener starts the local loopback TCP control server used by
// the factorio container's bash supervisor via `/dev/tcp/127.0.0.1:27016`.
func (s *SidecarServer) StartControlListener(ctx context.Context) (net.Addr, error) {
	if s.cfg.ControlAddr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", s.cfg.ControlAddr)
	if err != nil {
		return nil, fmt.Errorf("listen control %s: %w", s.cfg.ControlAddr, err)
	}
	s.controlActive.Store(true)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handleControlConn(conn)
		}
	}()
	slog.Info("Started local control TCP listener", "addr", ln.Addr().String())
	return ln.Addr(), nil
}

func (s *SidecarServer) handleControlConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return
	}
	cmd := strings.TrimSpace(line)

	switch {
	case cmd == "actor-name":
		_, _ = conn.Write([]byte(s.currentTargetActor() + "\n"))

	case cmd == "preset":
		spec := GetWorldSpec(s.currentTargetActor())
		_, _ = conn.Write([]byte(spec.MapGenPreset + "\n"))

	case cmd == "server-settings.json":
		spec := GetWorldSpec(s.currentTargetActor())
		b, err := spec.ServerSettingsJSON(s.cfg.RequireUserVerification)
		if err != nil {
			slog.Warn("Failed to marshal server-settings.json", "err", err)
			return
		}
		_, _ = conn.Write(b)

	case cmd == "map-gen-settings.json":
		spec := GetWorldSpec(s.currentTargetActor())
		b, err := spec.MapGenSettingsJSON()
		if err != nil {
			slog.Warn("Failed to marshal map-gen-settings.json", "err", err)
			return
		}
		_, _ = conn.Write(b)

	case strings.HasPrefix(cmd, "ready-new:"):
		actor := strings.TrimSpace(strings.TrimPrefix(cmd, "ready-new:"))
		s.onWorldReady(actor, true)
		_, _ = conn.Write([]byte("ok\n"))

	case strings.HasPrefix(cmd, "ready-existing:"), strings.HasPrefix(cmd, "ready:"):
		actor := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "ready-existing:"), "ready:"))
		s.onWorldReady(actor, false)
		_, _ = conn.Write([]byte("ok\n"))
	}
}

func (s *SidecarServer) currentTargetActor() string {
	if !s.activated.Load() {
		return "golden"
	}
	name := s.readSystemInfo("actor-name")
	if name == "" {
		return "golden"
	}
	return name
}

func (s *SidecarServer) onWorldReady(actor string, isNewMap bool) {
	if actor == "" {
		actor = "golden"
	}
	// Wait until Factorio's UDP listener is responding on 127.0.0.1:34197.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if s.probeFactorioUDP(150 * time.Millisecond) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if isNewMap && actor != "golden" {
		s.initWorldViaRCON(actor)
	}
	s.worldMu.Lock()
	s.readyActor = actor
	s.worldMu.Unlock()
	slog.Info("Factorio world ready", "actor", actor, "new_map", isNewMap)
}

func (s *SidecarServer) initWorldViaRCON(actor string) {
	spec := GetWorldSpec(actor)
	if spec.InitLua == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	luaCmd := "/c " + spec.InitLua
	// Factorio requires executing the first /c command twice to confirm
	// disabling achievements on the save.
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, luaCmd); err != nil {
			slog.Warn("RCON world init Lua failed", "actor", actor, "attempt", attempt, "err", err)
		}
	}
	if _, err := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/server-save"); err == nil {
		_, _ = rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/version")
	}
	slog.Info("Initialized new Factorio world via RCON", "actor", actor, "server_name", spec.ServerName, "preset", spec.MapGenPreset)
}

func (s *SidecarServer) isTargetWorldReady() bool {
	if !s.controlActive.Load() {
		return true
	}
	target := s.currentTargetActor()
	s.worldMu.RLock()
	ready := s.readyActor
	s.worldMu.RUnlock()
	return ready == target
}

func (s *SidecarServer) waitForTargetWorldReady(ctx context.Context, timeout time.Duration) bool {
	s.activated.Store(true)
	if s.isTargetWorldReady() {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if s.isTargetWorldReady() && s.probeFactorioUDP(150*time.Millisecond) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s.isTargetWorldReady()
}

// InitFactorioDir seeds /factorio/config and /factorio/mods on the shared
// durableDir volume and writes /factorio/config/.initialized once complete.
func (s *SidecarServer) InitFactorioDir() error {
	if s.cfg.FactorioDir == "" {
		return nil
	}
	for _, sub := range []string{"saves", "config", "mods", "scenarios", "script-output"} {
		if err := os.MkdirAll(filepath.Join(s.cfg.FactorioDir, sub), 0o777); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}

	configDir := filepath.Join(s.cfg.FactorioDir, "config")
	modsDir := filepath.Join(s.cfg.FactorioDir, "mods")

	configINI := "[path]\nread-data=__PATH__executable__/../../data\nwrite-data=" + s.cfg.FactorioDir + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.ini"), []byte(configINI), 0o644); err != nil {
		return fmt.Errorf("write config.ini: %w", err)
	}

	if err := os.WriteFile(filepath.Join(configDir, "rconpw"), []byte(s.cfg.RCONPassword+"\n"), 0o600); err != nil {
		return fmt.Errorf("write rconpw: %w", err)
	}

	serverSettingsPath := filepath.Join(configDir, "server-settings.json")
	if _, err := os.Stat(serverSettingsPath); os.IsNotExist(err) {
		spec := GetWorldSpec("golden")
		b, err := spec.ServerSettingsJSON(s.cfg.RequireUserVerification)
		if err != nil {
			return err
		}
		if err := os.WriteFile(serverSettingsPath, b, 0o644); err != nil {
			return fmt.Errorf("write server-settings.json: %w", err)
		}
	}

	adminListPath := filepath.Join(configDir, "server-adminlist.json")
	if _, err := os.Stat(adminListPath); os.IsNotExist(err) {
		if err := os.WriteFile(adminListPath, []byte("[\n  \"drewbr\",\n  \"drewmate\"\n]\n"), 0o644); err != nil {
			return fmt.Errorf("write server-adminlist.json: %w", err)
		}
	}

	modListPath := filepath.Join(modsDir, "mod-list.json")
	if _, err := os.Stat(modListPath); os.IsNotExist(err) {
		modList := `{
  "mods": [
    {"name": "base", "enabled": true},
    {"name": "elevated-rails", "enabled": true},
    {"name": "quality", "enabled": true},
    {"name": "space-age", "enabled": true}
  ]
}
`
		if err := os.WriteFile(modListPath, []byte(modList), 0o644); err != nil {
			return fmt.Errorf("write mod-list.json: %w", err)
		}
	}

	marker := filepath.Join(configDir, ".initialized")
	if err := os.WriteFile(marker, []byte("ok\n"), 0o644); err != nil {
		return fmt.Errorf("write .initialized marker: %w", err)
	}
	slog.Info("Initialized Factorio directory", "dir", s.cfg.FactorioDir)
	return nil
}

func (s *SidecarServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/prepare-suspend", s.handlePrepareSuspend)
	mux.HandleFunc("/status", s.handleStatus)
	return mux
}

// handleReadyz probes the local Factorio UDP server on 127.0.0.1:34197 so
// Substrate's Golden Snapshot waits until Factorio has finished map creation
// and opened its UDP listener.
func (s *SidecarServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.probeFactorioUDP(150 * time.Millisecond) {
		http.Error(w, "factorio UDP not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *SidecarServer) probeFactorioUDP(timeout time.Duration) bool {
	raddr, err := net.ResolveUDPAddr("udp4", s.cfg.FactorioUDPAddr)
	if err != nil {
		return false
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return false
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	probe := tunnelproto.BuildFactorioProbePacket(uint32(time.Now().UnixNano()))
	if _, err := conn.Write(probe); err != nil {
		return false
	}
	var buf [256]byte
	n, err := conn.Read(buf[:])
	if err != nil {
		return false
	}
	return tunnelproto.IsFactorioServerReply(buf[:n])
}

func (s *SidecarServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	s.activated.Store(true)

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("WebSocket upgrade failed", "err", err)
		return
	}

	s.wsMu.Lock()
	oldWS := s.activeWS
	s.activeWS = ws
	s.wsMu.Unlock()
	if oldWS != nil {
		_ = oldWS.Close()
	}

	slog.Info("Gateway WebSocket connected", "remote", r.RemoteAddr, "actor", s.currentTargetActor())
	defer func() {
		s.wsMu.Lock()
		if s.activeWS == ws {
			s.activeWS = nil
		}
		s.wsMu.Unlock()
		_ = ws.Close()
		slog.Info("Gateway WebSocket closed", "remote", r.RemoteAddr)
	}()

	// If this Actor just woke from the Golden Snapshot on its very first
	// connect, the factorio container is generating its custom map (~2.2s).
	// Coalesce incoming client ConnectionRequest retries per clientAddr
	// without setting a read deadline on gorilla/websocket (which would poison
	// the Conn on timeout), and flush the freshest packet per client as soon
	// as the custom map's UDP socket is live.
	var (
		pendingMu    sync.Mutex
		pending      = make(map[string][]byte)
		pendingOrder []string
		waitOnce     sync.Once
	)

	for {
		msgType, frame, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		clientAddr, payload, err := tunnelproto.DecodeFrame(frame)
		if err != nil {
			slog.Warn("Invalid tunnel frame", "err", err)
			continue
		}

		pendingMu.Lock()
		if !s.isTargetWorldReady() {
			if _, exists := pending[clientAddr]; !exists {
				pendingOrder = append(pendingOrder, clientAddr)
			}
			cp := make([]byte, len(payload))
			copy(cp, payload)
			pending[clientAddr] = cp
			waitOnce.Do(func() {
				slog.Info("Waiting for first-connect custom world generation before forwarding UDP", "actor", s.currentTargetActor())
				go func() {
					s.waitForTargetWorldReady(r.Context(), 45*time.Second)
					pendingMu.Lock()
					defer pendingMu.Unlock()
					for _, addr := range pendingOrder {
						s.forwardUDPToFactorio(addr, pending[addr])
					}
					pending = make(map[string][]byte)
					pendingOrder = nil
				}()
			})
			pendingMu.Unlock()
			continue
		}
		pendingMu.Unlock()

		s.forwardUDPToFactorio(clientAddr, payload)
	}
}

func (s *SidecarServer) forwardUDPToFactorio(clientAddr string, payload []byte) {
	s.packetsIn.Add(1)
	sess, err := s.getOrCreateSession(clientAddr)
	if err != nil {
		slog.Warn("Failed to create UDP session", "client", clientAddr, "err", err)
		return
	}
	sess.lastActive.Store(time.Now().UnixNano())
	if _, err := sess.conn.Write(payload); err != nil {
		slog.Warn("UDP write to Factorio failed", "client", clientAddr, "err", err)
	}
}

func (s *SidecarServer) getOrCreateSession(clientAddr string) (*clientSession, error) {
	s.mu.RLock()
	sess, ok := s.sessions[clientAddr]
	s.mu.RUnlock()
	if ok {
		return sess, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[clientAddr]; ok {
		return sess, nil
	}

	raddr, err := net.ResolveUDPAddr("udp4", s.cfg.FactorioUDPAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return nil, err
	}
	sess = &clientSession{
		clientAddr: clientAddr,
		conn:       conn,
	}
	sess.lastActive.Store(time.Now().UnixNano())
	s.sessions[clientAddr] = sess

	go s.readLoopForClient(sess)
	return sess, nil
}

func (s *SidecarServer) readLoopForClient(sess *clientSession) {
	defer func() {
		s.mu.Lock()
		if s.sessions[sess.clientAddr] == sess {
			delete(s.sessions, sess.clientAddr)
		}
		s.mu.Unlock()
		_ = sess.conn.Close()
	}()

	buf := make([]byte, 65535)
	for {
		_ = sess.conn.SetReadDeadline(time.Now().Add(s.cfg.SessionIdleTimeout))
		n, err := sess.conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				last := time.Unix(0, sess.lastActive.Load())
				if time.Since(last) >= s.cfg.SessionIdleTimeout {
					return
				}
				continue
			}
			if strings.Contains(err.Error(), "connection refused") {
				last := time.Unix(0, sess.lastActive.Load())
				if time.Since(last) >= s.cfg.SessionIdleTimeout {
					return
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		sess.lastActive.Store(time.Now().UnixNano())
		frame, err := tunnelproto.EncodeFrame(sess.clientAddr, buf[:n])
		if err != nil {
			continue
		}
		s.wsMu.Lock()
		ws := s.activeWS
		if ws != nil {
			_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err = ws.WriteMessage(websocket.BinaryMessage, frame)
		}
		s.wsMu.Unlock()
		if ws != nil && err == nil {
			s.packetsOut.Add(1)
		}
	}
}

func (s *SidecarServer) handlePrepareSuspend(w http.ResponseWriter, r *http.Request) {
	s.activated.Store(true)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	saveOut, err := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/server-save")
	if err == nil {
		// Execute a follow-up RCON round-trip after /server-save so we only return
		// once Factorio's main thread has finished writing and renaming *.tmp.zip.
		_, _ = rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/version")
	}
	resp := map[string]any{
		"actor":       s.readSystemInfo("actor-name"),
		"atespace":    s.readSystemInfo("atespace"),
		"save_output": strings.TrimSpace(saveOut),
	}
	if err != nil {
		resp["rcon_error"] = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *SidecarServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.waitForTargetWorldReady(r.Context(), 30*time.Second)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Optional RCON command override (e.g. /status?cmd=/c+...) for testing.
	customCmd := r.URL.Query().Get("cmd")
	var customOut string
	if customCmd != "" {
		out, err := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, customCmd)
		if err != nil {
			customOut = "error: " + err.Error()
		} else {
			customOut = strings.TrimSpace(out)
		}
	}

	version, _ := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/version")
	seed, _ := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/seed")
	gameTime, _ := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/time")
	players, _ := rcon.Execute(ctx, s.cfg.FactorioRCONAddr, s.cfg.RCONPassword, "/players")

	s.mu.RLock()
	activeSessions := len(s.sessions)
	s.mu.RUnlock()

	actorName := s.readSystemInfo("actor-name")
	spec := GetWorldSpec(actorName)

	resp := map[string]any{
		"actor":           actorName,
		"atespace":        s.readSystemInfo("atespace"),
		"uid":             s.readSystemInfo("actor-uid"),
		"server_name":     spec.ServerName,
		"map_preset":      spec.MapGenPreset,
		"world_ready":     s.isTargetWorldReady(),
		"version":         strings.TrimSpace(version),
		"seed":            strings.TrimSpace(seed),
		"game_time":       strings.TrimSpace(gameTime),
		"players":         strings.TrimSpace(players),
		"active_sessions": activeSessions,
		"packets_in":      s.packetsIn.Load(),
		"packets_out":     s.packetsOut.Load(),
	}
	if customCmd != "" {
		resp["cmd_output"] = customOut
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *SidecarServer) readSystemInfo(file string) string {
	b, err := os.ReadFile(filepath.Join(s.cfg.SystemInfoDir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.ListenAddr, "listen-addr", ":80", "HTTP/WebSocket listen address inside the actor")
	flag.StringVar(&cfg.ControlAddr, "control-addr", "127.0.0.1:27016", "Local loopback TCP control address for factorio container coordination")
	flag.StringVar(&cfg.FactorioUDPAddr, "factorio-udp-addr", "127.0.0.1:34197", "Local Factorio UDP address")
	flag.StringVar(&cfg.FactorioRCONAddr, "factorio-rcon-addr", "127.0.0.1:27015", "Local Factorio RCON TCP address")
	flag.StringVar(&cfg.FactorioDir, "factorio-dir", "/factorio", "Path to shared Factorio durableDir volume")
	flag.StringVar(&cfg.SystemInfoDir, "system-info-dir", "/run/ate", "Path to Substrate systemInfo volume")
	flag.StringVar(&cfg.RCONPassword, "rcon-password", "substrate-factorio-rcon", "Local RCON password")
	flag.BoolVar(&cfg.RequireUserVerification, "require-user-verification", false, "Whether Factorio requires factorio.com user verification")
	flag.DurationVar(&cfg.SessionIdleTimeout, "session-idle-timeout", 2*time.Minute, "Idle timeout for local per-client UDP sockets")
	flag.Parse()

	srv := NewSidecarServer(cfg)
	if err := srv.InitFactorioDir(); err != nil {
		slog.Error("Failed to initialize Factorio directory", "err", err)
		os.Exit(1)
	}
	if _, err := srv.StartControlListener(context.Background()); err != nil {
		slog.Error("Failed to start local control listener", "err", err)
		os.Exit(1)
	}

	slog.Info("Starting factorio-sidecar", "listen", cfg.ListenAddr, "control", cfg.ControlAddr, "factorio_udp", cfg.FactorioUDPAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, srv.Handler()); err != nil {
		slog.Error("factorio-sidecar exited", "err", err)
		os.Exit(1)
	}
}
