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

// WorkerPoolStatus describes the spec and ready replica counts for a WorkerPool.
type WorkerPoolStatus struct {
	SpecReplicas  int32 `json:"spec_replicas"`
	ReadyReplicas int32 `json:"ready_replicas"`
}

// WorkerPoolScaler scales the underlying Kubernetes WorkerPool CR on demand
// (e.g. 0 -> 1 on inbound Factorio UDP traffic, 1 -> 0 after all worlds are idle).
type WorkerPoolScaler interface {
	GetWorkerPoolStatus(ctx context.Context, namespace, name string) (WorkerPoolStatus, error)
	SetWorkerPoolReplicas(ctx context.Context, namespace, name string, replicas int32) (WorkerPoolStatus, error)
}

type k8sWorkerPoolScaler struct {
	baseURL    string
	tokenPath  string
	httpClient *http.Client
}

func newInClusterWorkerPoolScaler() (WorkerPoolScaler, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, nil
	}
	caPath := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read k8s ca %q: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certs found in k8s ca %q", caPath)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: pool,
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	return &k8sWorkerPoolScaler{
		baseURL:    fmt.Sprintf("https://%s:%s", host, port),
		tokenPath:  tokenPath,
		httpClient: client,
	}, nil
}

func (s *k8sWorkerPoolScaler) getToken() (string, error) {
	tokenBytes, err := os.ReadFile(s.tokenPath)
	if err != nil {
		return "", fmt.Errorf("read k8s serviceaccount token %q: %w", s.tokenPath, err)
	}
	return strings.TrimSpace(string(tokenBytes)), nil
}

func (s *k8sWorkerPoolScaler) GetWorkerPoolStatus(ctx context.Context, namespace, name string) (WorkerPoolStatus, error) {
	token, err := s.getToken()
	if err != nil {
		return WorkerPoolStatus{}, err
	}
	u := fmt.Sprintf("%s/apis/ate.dev/v1alpha1/namespaces/%s/workerpools/%s", s.baseURL, url.PathEscape(namespace), url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return WorkerPoolStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return WorkerPoolStatus{}, fmt.Errorf("get workerpool %s/%s: %w", namespace, name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return WorkerPoolStatus{}, fmt.Errorf("get workerpool %s/%s status %d: %s", namespace, name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Spec struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int32 `json:"readyReplicas"`
			Replicas      int32 `json:"replicas"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return WorkerPoolStatus{}, fmt.Errorf("decode workerpool response: %w", err)
	}

	return WorkerPoolStatus{
		SpecReplicas:  payload.Spec.Replicas,
		ReadyReplicas: payload.Status.ReadyReplicas,
	}, nil
}

func (s *k8sWorkerPoolScaler) SetWorkerPoolReplicas(ctx context.Context, namespace, name string, replicas int32) (WorkerPoolStatus, error) {
	token, err := s.getToken()
	if err != nil {
		return WorkerPoolStatus{}, err
	}
	u := fmt.Sprintf("%s/apis/ate.dev/v1alpha1/namespaces/%s/workerpools/%s", s.baseURL, url.PathEscape(namespace), url.PathEscape(name))
	patchBody := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, strings.NewReader(patchBody))
	if err != nil {
		return WorkerPoolStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return WorkerPoolStatus{}, fmt.Errorf("patch workerpool %s/%s: %w", namespace, name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return WorkerPoolStatus{}, fmt.Errorf("patch workerpool %s/%s status %d: %s", namespace, name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Spec struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int32 `json:"readyReplicas"`
			Replicas      int32 `json:"replicas"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return WorkerPoolStatus{}, fmt.Errorf("decode workerpool patch response: %w", err)
	}

	return WorkerPoolStatus{
		SpecReplicas:  payload.Spec.Replicas,
		ReadyReplicas: payload.Status.ReadyReplicas,
	}, nil
}

// TemplateSnapshotInfo describes the current state of an ActorTemplate in
// Agent Substrate.
type TemplateSnapshotInfo struct {
	UID         string `json:"uid"`
	ImageRef    string `json:"image_ref"`
	SnapshotURI string `json:"snapshot_uri"`
}

// TemplateManager is an optional extension implemented by ActorSuspender that
// queries and replaces the Factorio ActorTemplate in-place when a new
// factoriotools/factorio:stable image digest is published.
type TemplateManager interface {
	GetTemplateInfo(ctx context.Context, atespace, templateName string) (TemplateSnapshotInfo, error)
	UpgradeTemplateImage(ctx context.Context, atespace, templateName, newImageRef string) (TemplateSnapshotInfo, error)
}

// UpstreamRelease represents a resolved container image release from Docker Hub.
type UpstreamRelease struct {
	ImageRef string `json:"image_ref"`
	Digest   string `json:"digest"`
	Version  string `json:"version"`
}

// RegistryClient resolves the latest container image digest and Factorio
// version tag (e.g. "2.1.24") from an upstream container registry.
type RegistryClient interface {
	ResolveLatestRelease(ctx context.Context, repo, tag string) (UpstreamRelease, error)
}

type dockerHubRegistryClient struct {
	baseURL    string
	httpClient *http.Client
}

func newDockerHubRegistryClient(baseURL string) RegistryClient {
	if baseURL == "" {
		baseURL = "https://hub.docker.com"
	}
	return &dockerHubRegistryClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (c *dockerHubRegistryClient) ResolveLatestRelease(ctx context.Context, repo, tag string) (UpstreamRelease, error) {
	if repo == "" {
		repo = "factoriotools/factorio"
	}
	if tag == "" {
		tag = "stable"
	}
	u := fmt.Sprintf("%s/v2/repositories/%s/tags?page_size=25", c.baseURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return UpstreamRelease{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return UpstreamRelease{}, fmt.Errorf("query Docker Hub tags %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return UpstreamRelease{}, fmt.Errorf("Docker Hub status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Results []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return UpstreamRelease{}, fmt.Errorf("decode Docker Hub tags response: %w", err)
	}

	var targetDigest string
	for _, r := range payload.Results {
		if r.Name == tag && r.Digest != "" {
			targetDigest = r.Digest
			break
		}
	}
	if targetDigest == "" {
		return UpstreamRelease{}, fmt.Errorf("tag %q not found in %s", tag, repo)
	}

	var version string
	for _, r := range payload.Results {
		if r.Digest == targetDigest && r.Name != tag && strings.Count(r.Name, ".") == 2 {
			version = r.Name
			break
		}
	}

	return UpstreamRelease{
		ImageRef: fmt.Sprintf("index.docker.io/%s@%s", repo, targetDigest),
		Digest:   targetDigest,
		Version:  version,
	}, nil
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

func (g *grpcActorSuspender) GetTemplateInfo(ctx context.Context, atespace, templateName string) (TemplateSnapshotInfo, error) {
	tmpl, err := g.client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     templateName,
		},
	})
	if err != nil {
		return TemplateSnapshotInfo{}, err
	}
	return extractTemplateInfo(tmpl), nil
}

func (g *grpcActorSuspender) UpgradeTemplateImage(ctx context.Context, atespace, templateName, newImageRef string) (TemplateSnapshotInfo, error) {
	tmpl, err := g.client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     templateName,
		},
	})
	if err != nil {
		return TemplateSnapshotInfo{}, fmt.Errorf("get ActorTemplate %s/%s: %w", atespace, templateName, err)
	}

	updated := false
	for _, c := range tmpl.GetContainers() {
		if c.GetName() == "factorio" {
			c.Image = newImageRef
			updated = true
		}
	}
	if !updated {
		return TemplateSnapshotInfo{}, fmt.Errorf("container 'factorio' not found in ActorTemplate %s/%s", atespace, templateName)
	}

	if _, err := g.client.DeleteActorTemplate(ctx, &ateapipb.DeleteActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     templateName,
		},
	}); err != nil {
		return TemplateSnapshotInfo{}, fmt.Errorf("delete old ActorTemplate %s/%s: %w", atespace, templateName, err)
	}

	if tmpl.Metadata != nil {
		tmpl.Metadata.Uid = ""
		tmpl.Metadata.Version = 0
		tmpl.Metadata.CreateTime = nil
		tmpl.Metadata.UpdateTime = nil
	}
	tmpl.Status = nil

	created, err := g.client.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{
		ActorTemplate: tmpl,
	})
	if err != nil {
		return TemplateSnapshotInfo{}, fmt.Errorf("recreate ActorTemplate %s/%s with image %s: %w", atespace, templateName, newImageRef, err)
	}

	// Wait for the new Golden Snapshot to finish building so Actors can resume immediately.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return extractTemplateInfo(created), fmt.Errorf("waiting for Golden Snapshot on %s/%s: %w", atespace, templateName, ctx.Err())
		case <-ticker.C:
			cur, err := g.client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
				ActorTemplate: &ateapipb.ObjectRef{
					Atespace: atespace,
					Name:     templateName,
				},
			})
			if err != nil {
				continue
			}
			gs := cur.GetStatus().GetGoldenSnapshotStatus()
			if errMsg := gs.GetErrorMessage(); errMsg != "" {
				return extractTemplateInfo(cur), fmt.Errorf("Golden Snapshot failed for %s/%s: %s", atespace, templateName, errMsg)
			}
			if uri := gs.GetGoldenSnapshot().GetSnapshotUri(); uri != "" {
				return extractTemplateInfo(cur), nil
			}
		}
	}
}

func extractTemplateInfo(tmpl *ateapipb.ActorTemplate) TemplateSnapshotInfo {
	var img string
	for _, c := range tmpl.GetContainers() {
		if c.GetName() == "factorio" {
			img = c.GetImage()
			break
		}
	}
	return TemplateSnapshotInfo{
		UID:         tmpl.GetMetadata().GetUid(),
		ImageRef:    img,
		SnapshotURI: tmpl.GetStatus().GetGoldenSnapshotStatus().GetGoldenSnapshot().GetSnapshotUri(),
	}
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

	// Automatic ActorTemplate upgrade settings (proactive + reactive).
	TemplateAtespace  string
	TemplateName      string
	UpstreamRepo      string
	UpstreamTag       string
	ImagePollInterval time.Duration
	Registry          RegistryClient

	// Scale-to-Zero WorkerPool lifecycle settings.
	WorkerPoolNamespace   string
	WorkerPoolName        string
	WorkerPoolIdleTimeout time.Duration
	ResumeTimeout         time.Duration
	Scaler                WorkerPoolScaler
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
	lastClientVersion string
	lastServerVersion string

	packetsRx      atomic.Uint64
	packetsTx      atomic.Uint64
	scannerDropped atomic.Uint64
	wakeCount      atomic.Uint64
	suspendCount   atomic.Uint64
}

type Gateway struct {
	cfg       GatewayConfig
	suspender ActorSuspender
	tmplMgr   TemplateManager
	registry  RegistryClient
	scaler    WorkerPoolScaler
	worlds    map[string]*WorldTunnel // keyed by ActorRef ("atespace/actor")
	byPort    map[int]*WorldTunnel
	preemptMu sync.Mutex
	upgradeMu sync.Mutex
	scaleMu   sync.Mutex

	metaMu             sync.Mutex
	templateInfo       TemplateSnapshotInfo
	upstreamRelease    UpstreamRelease
	knownServerVersion string
	lastClientVersion  string
	lastUpdateCheck    time.Time
	lastUpgradeTime    time.Time
	lastUpgradeReason  string
	lastUpdateError    string
	upgradeCount       atomic.Uint64

	upgradeInProgress atomic.Bool
	workerPoolStatus  WorkerPoolStatus
	lastScaleCheck    time.Time
	lastScaleTime     time.Time
	lastScaleReason   string
	scaleUpCount      atomic.Uint64
	scaleDownCount    atomic.Uint64
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
	if cfg.TemplateAtespace == "" {
		cfg.TemplateAtespace = "factorio"
	}
	if cfg.TemplateName == "" {
		cfg.TemplateName = "factorio-server"
	}
	if cfg.UpstreamRepo == "" {
		cfg.UpstreamRepo = "factoriotools/factorio"
	}
	if cfg.UpstreamTag == "" {
		cfg.UpstreamTag = "stable"
	}
	if cfg.WorkerPoolNamespace == "" {
		cfg.WorkerPoolNamespace = "factorio"
	}
	if cfg.WorkerPoolName == "" {
		cfg.WorkerPoolName = "factorio"
	}
	if cfg.WorkerPoolIdleTimeout <= 0 {
		cfg.WorkerPoolIdleTimeout = 2 * time.Minute
	}
	if cfg.ResumeTimeout <= 0 {
		cfg.ResumeTimeout = 5 * time.Minute
	}
	var tmplMgr TemplateManager
	if tm, ok := suspender.(TemplateManager); ok {
		tmplMgr = tm
	}
	reg := cfg.Registry
	if reg == nil && tmplMgr != nil && cfg.ImagePollInterval > 0 {
		reg = newDockerHubRegistryClient("")
	}
	gw := &Gateway{
		cfg:       cfg,
		suspender: suspender,
		tmplMgr:   tmplMgr,
		registry:  reg,
		scaler:    cfg.Scaler,
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
	if gw.tmplMgr != nil && gw.registry != nil {
		go gw.syncTemplateMetadata(ctx)
		if gw.cfg.ImagePollInterval > 0 {
			go gw.imageUpdateLoop(ctx)
		}
	}
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
	clientVer, hasClientVer := tunnelproto.ParseClientVersion(pkt)

	wt.mu.Lock()
	wt.clientAddrs[clientKey] = raddr
	if hasClientVer {
		wt.lastClientVersion = clientVer
		wt.gw.recordClientVersion(clientVer)
	}
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
				"client_version", clientVer,
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

	// Reactive client version check while world is already RUNNING: if a newly
	// updated Steam client connects with a newer version than the running server,
	// trigger a background check/upgrade when the world is idle.
	if hasClientVer && wt.gw.isClientNewerThanServer(clientVer) {
		go wt.gw.handleReactiveVersionMismatch(ctx, wt, clientVer)
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
	// Ensure the underlying WorkerPool is scaled up from 0 to 1 so a worker pod can schedule.
	wt.gw.ensureWorkerPoolScaledUp(ctx, "wake-"+wt.spec.Actor)

	// If the WorkerPool is full (e.g. 1 replica) and another world is currently
	// running or resuming without active player traffic, suspend it first so
	// this world can claim the worker pod.
	wt.gw.preemptIdleWorldsFor(ctx, wt)

	// Reactive auto-upgrade before wake: if the waking client packet advertised a
	// newer Factorio version than the current ActorTemplate (e.g. Steam auto-updated
	// the client before the 30m background poll fired), check Docker Hub and
	// upgrade the ActorTemplate before dialing atenet-router!
	wt.gw.maybeUpgradeBeforeWake(ctx, wt)

	tunnelCtx, cancel := context.WithCancel(ctx)
	wt.mu.Lock()
	wt.wsCancel = cancel
	wt.mu.Unlock()

	go wt.maintainWebSocketLoop(tunnelCtx)
}

func (gw *Gateway) ensureWorkerPoolScaledUp(ctx context.Context, reason string) {
	if gw.scaler == nil {
		return
	}
	gw.scaleMu.Lock()
	defer gw.scaleMu.Unlock()

	cur, err := gw.scaler.GetWorkerPoolStatus(ctx, gw.cfg.WorkerPoolNamespace, gw.cfg.WorkerPoolName)
	if err != nil {
		slog.Warn("Failed to get WorkerPool status", "namespace", gw.cfg.WorkerPoolNamespace, "name", gw.cfg.WorkerPoolName, "err", err)
		return
	}
	gw.workerPoolStatus = cur
	gw.lastScaleCheck = time.Now()

	wanted := int32(gw.cfg.MaxActiveWorlds)
	if cur.SpecReplicas < wanted {
		slog.Info("Scaling up WorkerPool from 0 -> 1",
			"namespace", gw.cfg.WorkerPoolNamespace,
			"name", gw.cfg.WorkerPoolName,
			"old_spec_replicas", cur.SpecReplicas,
			"new_spec_replicas", wanted,
			"reason", reason,
		)
		updated, err := gw.scaler.SetWorkerPoolReplicas(ctx, gw.cfg.WorkerPoolNamespace, gw.cfg.WorkerPoolName, wanted)
		if err != nil {
			slog.Error("Failed to scale up WorkerPool", "namespace", gw.cfg.WorkerPoolNamespace, "name", gw.cfg.WorkerPoolName, "err", err)
			return
		}
		gw.scaleUpCount.Add(1)
		gw.workerPoolStatus = updated
		gw.lastScaleTime = time.Now()
		gw.lastScaleReason = reason
	}
}

func (gw *Gateway) maybeScaleDownWorkerPool(ctx context.Context, now time.Time, suspendedFor time.Duration) {
	if gw.scaler == nil || gw.cfg.WorkerPoolIdleTimeout <= 0 {
		return
	}
	gw.scaleMu.Lock()
	defer gw.scaleMu.Unlock()

	if gw.workerPoolStatus.SpecReplicas == 0 && now.Sub(gw.lastScaleCheck) < 10*time.Second {
		return
	}

	cur, err := gw.scaler.GetWorkerPoolStatus(ctx, gw.cfg.WorkerPoolNamespace, gw.cfg.WorkerPoolName)
	if err != nil {
		slog.Warn("Failed to check WorkerPool status for scale down", "err", err)
		return
	}
	gw.workerPoolStatus = cur
	gw.lastScaleCheck = now

	if cur.SpecReplicas > 0 {
		slog.Info("All Factorio worlds suspended for WorkerPool idle timeout; scaling WorkerPool to 0",
			"namespace", gw.cfg.WorkerPoolNamespace,
			"name", gw.cfg.WorkerPoolName,
			"suspended_for", suspendedFor.Round(time.Second),
			"idle_timeout", gw.cfg.WorkerPoolIdleTimeout,
		)
		updated, err := gw.scaler.SetWorkerPoolReplicas(ctx, gw.cfg.WorkerPoolNamespace, gw.cfg.WorkerPoolName, 0)
		if err != nil {
			slog.Error("Failed to scale down WorkerPool to 0", "err", err)
			return
		}
		gw.scaleDownCount.Add(1)
		gw.workerPoolStatus = updated
		gw.lastScaleTime = now
		gw.lastScaleReason = fmt.Sprintf("idle-timeout-%s", suspendedFor.Round(time.Second))
	}
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
	resumeStart := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		wt.mu.Lock()
		st := wt.state
		wt.mu.Unlock()
		if st == WorldStateResuming && time.Since(resumeStart) > wt.gw.cfg.ResumeTimeout {
			slog.Error("Timed out waiting for Actor to resume via atenet-router",
				"actor", wt.spec.ActorRef(),
				"timeout", wt.gw.cfg.ResumeTimeout,
			)
			wt.mu.Lock()
			wt.state = WorldStateSuspended
			wt.lastStateChange = time.Now()
			wt.mu.Unlock()
			return
		}

		ws, err := wt.dialActorWebSocket(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wt.mu.Lock()
			st := wt.state
			wt.mu.Unlock()
			if st == WorldStateResuming && time.Since(resumeStart) > wt.gw.cfg.ResumeTimeout {
				slog.Error("Timed out waiting for Actor to resume via atenet-router after dial error",
					"actor", wt.spec.ActorRef(),
					"err", err,
					"timeout", wt.gw.cfg.ResumeTimeout,
				)
				wt.mu.Lock()
				wt.state = WorldStateSuspended
				wt.lastStateChange = time.Now()
				wt.mu.Unlock()
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
			wt.lastResumeLatency = now.Sub(resumeStart)
			wt.state = WorldStateRunning
			wt.lastStateChange = now
			wt.lastOutboundReply = now // Unconditionally reset so the world has a fresh idle timeout window!
			slog.Info("Factorio Actor resumed and WebSocket tunnel established",
				"actor", wt.spec.ActorRef(),
				"resume_latency", wt.lastResumeLatency.Round(time.Millisecond),
			)
		}
		wt.ws = ws

		// Flush buffered client handshake packets if fresh (within 15s).
		// Older packets from before cold node boot have stale sequence numbers.
		for clientKey, pkt := range wt.pendingByClient {
			if now.Sub(wt.lastInbound) <= 15*time.Second {
				if frame, err := tunnelproto.EncodeFrame(clientKey, pkt); err == nil {
					_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
					_ = ws.WriteMessage(websocket.BinaryMessage, frame)
				}
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
		st = wt.state
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
		srvVer, hasSrvVer := tunnelproto.ParseServerReplyVersion(payload)

		if hasSrvVer {
			wt.mu.Lock()
			wt.lastServerVersion = srvVer
			wt.mu.Unlock()
			wt.gw.recordServerVersion(srvVer)
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

func (gw *Gateway) recordClientVersion(v string) {
	gw.metaMu.Lock()
	gw.lastClientVersion = v
	gw.metaMu.Unlock()
}

func (gw *Gateway) recordServerVersion(v string) {
	gw.metaMu.Lock()
	gw.knownServerVersion = v
	gw.metaMu.Unlock()
}

func (gw *Gateway) isClientNewerThanServer(clientVer string) bool {
	if clientVer == "" {
		return false
	}
	gw.metaMu.Lock()
	srvVer := gw.knownServerVersion
	gw.metaMu.Unlock()
	if srvVer == "" {
		return false
	}
	return tunnelproto.CompareVersions(clientVer, srvVer) > 0
}

func (gw *Gateway) syncTemplateMetadata(ctx context.Context) {
	if gw.tmplMgr == nil {
		return
	}
	syncCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	info, err := gw.tmplMgr.GetTemplateInfo(syncCtx, gw.cfg.TemplateAtespace, gw.cfg.TemplateName)
	if err == nil {
		gw.metaMu.Lock()
		gw.templateInfo = info
		gw.metaMu.Unlock()
	}
	if gw.registry != nil {
		rel, err := gw.registry.ResolveLatestRelease(syncCtx, gw.cfg.UpstreamRepo, gw.cfg.UpstreamTag)
		if err == nil {
			gw.metaMu.Lock()
			gw.upstreamRelease = rel
			gw.lastUpdateCheck = time.Now()
			if gw.knownServerVersion == "" && rel.Digest != "" && strings.Contains(info.ImageRef, rel.Digest) {
				gw.knownServerVersion = rel.Version
			}
			gw.metaMu.Unlock()
		}
	}
}

func (gw *Gateway) imageUpdateLoop(ctx context.Context) {
	ticker := time.NewTicker(gw.cfg.ImagePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if gw.allWorldsSuspended(nil) {
				_, _ = gw.CheckAndUpgradeTemplate(ctx, "proactive-background-poll", nil)
			}
		}
	}
}

func (gw *Gateway) allWorldsSuspended(ignore *WorldTunnel) bool {
	for _, wt := range gw.worlds {
		if wt == ignore {
			continue
		}
		wt.mu.Lock()
		st := wt.state
		wt.mu.Unlock()
		if st != WorldStateSuspended {
			return false
		}
	}
	return true
}

func (gw *Gateway) maybeUpgradeBeforeWake(ctx context.Context, wakingWorld *WorldTunnel) {
	if gw.tmplMgr == nil || gw.registry == nil {
		return
	}
	wakingWorld.mu.Lock()
	clientVer := wakingWorld.lastClientVersion
	wakingWorld.mu.Unlock()
	if clientVer == "" {
		return
	}

	gw.metaMu.Lock()
	srvVer := gw.knownServerVersion
	gw.metaMu.Unlock()

	// Only block wake-up if we know the current server version and the connecting
	// client is strictly newer (e.g. Steam updated the client from 2.1.24 to 2.1.25).
	if srvVer != "" && tunnelproto.CompareVersions(clientVer, srvVer) > 0 {
		slog.Info("Client version is newer than server version; triggering pre-wake Docker Hub upgrade",
			"actor", wakingWorld.spec.ActorRef(),
			"client_version", clientVer,
			"known_server_version", srvVer,
		)
		_, _ = gw.CheckAndUpgradeTemplate(ctx, "reactive-client-version:"+clientVer, wakingWorld)
	}
}

func (gw *Gateway) handleReactiveVersionMismatch(ctx context.Context, wt *WorldTunnel, clientVer string) {
	if gw.tmplMgr == nil || gw.registry == nil {
		return
	}
	if !gw.isClientNewerThanServer(clientVer) {
		return
	}
	// Never interrupt a world that is actively exchanging UDP packets (e.g. map download or active gameplay).
	wt.mu.Lock()
	idleFor := time.Since(wt.lastOutboundReply)
	wt.mu.Unlock()
	if idleFor < gw.cfg.PreemptIdleGrace {
		return
	}
	slog.Info("Detected newer Factorio client version against idle world; suspending and upgrading",
		"actor", wt.spec.ActorRef(),
		"client_version", clientVer,
	)
	_ = wt.suspendNow(ctx, "client-version-upgrade-"+clientVer)
	_, _ = gw.CheckAndUpgradeTemplate(ctx, "reactive-client-version:"+clientVer, nil)
}

// CheckAndUpgradeTemplate queries the upstream registry and current
// ActorTemplate, and if the upstream digest has changed while worlds are
// suspended, replaces the ActorTemplate in-place and waits for the new Golden
// Snapshot. Existing Actors retain their /factorio saves and automatically
// migrate via Agent Substrate's SNAPSHOT_SCOPE_DATA on next resume.
func (gw *Gateway) CheckAndUpgradeTemplate(ctx context.Context, reason string, ignoreWorld *WorldTunnel) (map[string]any, error) {
	if gw.tmplMgr == nil || gw.registry == nil {
		return map[string]any{"status": "disabled"}, nil
	}

	gw.upgradeMu.Lock()
	defer gw.upgradeMu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	tmplInfo, err := gw.tmplMgr.GetTemplateInfo(checkCtx, gw.cfg.TemplateAtespace, gw.cfg.TemplateName)
	if err != nil {
		gw.metaMu.Lock()
		gw.lastUpdateError = err.Error()
		gw.metaMu.Unlock()
		return nil, fmt.Errorf("get template info: %w", err)
	}

	upstream, err := gw.registry.ResolveLatestRelease(checkCtx, gw.cfg.UpstreamRepo, gw.cfg.UpstreamTag)
	if err != nil {
		gw.metaMu.Lock()
		gw.lastUpdateError = err.Error()
		gw.metaMu.Unlock()
		return nil, fmt.Errorf("resolve upstream release: %w", err)
	}

	now := time.Now()
	gw.metaMu.Lock()
	gw.templateInfo = tmplInfo
	gw.upstreamRelease = upstream
	gw.lastUpdateCheck = now
	gw.lastUpdateError = ""
	if gw.knownServerVersion == "" && upstream.Digest != "" && strings.Contains(tmplInfo.ImageRef, upstream.Digest) {
		gw.knownServerVersion = upstream.Version
	}
	gw.metaMu.Unlock()

	needsUpgrade := upstream.Digest != "" && !strings.Contains(tmplInfo.ImageRef, upstream.Digest)
	if !needsUpgrade {
		return map[string]any{
			"upgraded":         false,
			"reason":           reason,
			"template_uid":     tmplInfo.UID,
			"template_image":   tmplInfo.ImageRef,
			"upstream_image":   upstream.ImageRef,
			"upstream_digest":  upstream.Digest,
			"upstream_version": upstream.Version,
		}, nil
	}

	if !gw.allWorldsSuspended(ignoreWorld) {
		msg := "upstream update available; deferring until all worlds are SUSPENDED"
		slog.Info(msg, "current_image", tmplInfo.ImageRef, "upstream_image", upstream.ImageRef, "upstream_version", upstream.Version)
		return map[string]any{
			"upgraded":         false,
			"deferred":         true,
			"message":          msg,
			"template_image":   tmplInfo.ImageRef,
			"upstream_image":   upstream.ImageRef,
			"upstream_version": upstream.Version,
		}, nil
	}

	gw.upgradeInProgress.Store(true)
	defer gw.upgradeInProgress.Store(false)

	// Golden Snapshot building requires a ready WorkerPool pod.
	gw.ensureWorkerPoolScaledUp(checkCtx, "template-upgrade:"+reason)

	slog.Info("Upgrading Factorio ActorTemplate in-place to new upstream release",
		"atespace", gw.cfg.TemplateAtespace,
		"template", gw.cfg.TemplateName,
		"old_uid", tmplInfo.UID,
		"old_image", tmplInfo.ImageRef,
		"new_image", upstream.ImageRef,
		"new_version", upstream.Version,
		"reason", reason,
	)

	newInfo, err := gw.tmplMgr.UpgradeTemplateImage(checkCtx, gw.cfg.TemplateAtespace, gw.cfg.TemplateName, upstream.ImageRef)
	if err != nil {
		gw.metaMu.Lock()
		gw.lastUpdateError = err.Error()
		gw.metaMu.Unlock()
		slog.Error("ActorTemplate auto-upgrade failed", "err", err)
		return nil, err
	}

	gw.upgradeCount.Add(1)
	gw.metaMu.Lock()
	gw.templateInfo = newInfo
	if upstream.Version != "" {
		gw.knownServerVersion = upstream.Version
	}
	gw.lastUpgradeTime = time.Now()
	gw.lastUpgradeReason = reason
	gw.metaMu.Unlock()

	slog.Info("Factorio ActorTemplate Golden Snapshot ready on new release; Actors will auto-migrate via SNAPSHOT_SCOPE_DATA on next wake",
		"new_uid", newInfo.UID,
		"new_image", newInfo.ImageRef,
		"new_version", upstream.Version,
		"snapshot_uri", newInfo.SnapshotURI,
	)

	return map[string]any{
		"upgraded":         true,
		"reason":           reason,
		"old_template_uid": tmplInfo.UID,
		"new_template_uid": newInfo.UID,
		"old_image":        tmplInfo.ImageRef,
		"new_image":        newInfo.ImageRef,
		"upstream_version": upstream.Version,
		"snapshot_uri":     newInfo.SnapshotURI,
	}, nil
}

func (gw *Gateway) idleReaperLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			allSuspended := true
			var latestSuspendedChange time.Time

			for _, wt := range gw.worlds {
				wt.mu.Lock()
				st := wt.state
				lastActivity := wt.lastOutboundReply
				if lastActivity.IsZero() {
					lastActivity = wt.lastStateChange
				}
				changeTime := wt.lastStateChange
				wt.mu.Unlock()

				if st == WorldStateRunning {
					allSuspended = false
					idleDuration := now.Sub(lastActivity)
					if idleDuration >= gw.cfg.IdleTimeout {
						slog.Info("No Factorio UDP traffic for idle timeout; suspending Actor",
							"actor", wt.spec.ActorRef(),
							"idle_duration", idleDuration.Round(time.Second),
						)
						_ = wt.suspendNow(ctx, "idle-timeout")
					}
				} else if st != WorldStateSuspended {
					allSuspended = false
				} else {
					if changeTime.After(latestSuspendedChange) {
						latestSuspendedChange = changeTime
					}
				}
			}

			if allSuspended && !gw.upgradeInProgress.Load() {
				// Do not scale down WorkerPool if the ActorTemplate's Golden Snapshot is not ready yet!
				if gw.tmplMgr != nil {
					info, err := gw.tmplMgr.GetTemplateInfo(ctx, gw.cfg.TemplateAtespace, gw.cfg.TemplateName)
					if err == nil && info.SnapshotURI == "" {
						gw.ensureWorkerPoolScaledUp(ctx, "template-building")
						continue
					}
				}

				gw.metaMu.Lock()
				lastUpgrade := gw.lastUpgradeTime
				gw.metaMu.Unlock()
				if lastUpgrade.After(latestSuspendedChange) {
					latestSuspendedChange = lastUpgrade
				}
				if latestSuspendedChange.IsZero() {
					latestSuspendedChange = now
				}
				suspendedFor := now.Sub(latestSuspendedChange)
				if suspendedFor >= gw.cfg.WorkerPoolIdleTimeout {
					gw.maybeScaleDownWorkerPool(ctx, now, suspendedFor)
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
	wt.lastOutboundReply = time.Time{}
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
	mux.HandleFunc("/scale", gw.handleManualScale)
	mux.HandleFunc("/check-update", gw.handleCheckUpdate)
	return mux
}

func (gw *Gateway) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
		return
	}
	res, err := gw.CheckAndUpgradeTemplate(r.Context(), "manual-http-trigger", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
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
		clientVer := wt.lastClientVersion
		srvVer := wt.lastServerVersion
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
			"last_client_version":    clientVer,
			"last_server_version":    srvVer,
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

	gw.metaMu.Lock()
	tmplInfo := gw.templateInfo
	upstream := gw.upstreamRelease
	knownSrvVer := gw.knownServerVersion
	lastCliVer := gw.lastClientVersion
	lastCheck := gw.lastUpdateCheck
	lastUpgrade := gw.lastUpgradeTime
	lastReason := gw.lastUpgradeReason
	lastErr := gw.lastUpdateError
	gw.metaMu.Unlock()

	autoUpdate := map[string]any{
		"poll_interval":        gw.cfg.ImagePollInterval.String(),
		"template_atespace":    gw.cfg.TemplateAtespace,
		"template_name":        gw.cfg.TemplateName,
		"template_uid":         tmplInfo.UID,
		"template_image":       tmplInfo.ImageRef,
		"upstream_repo":        gw.cfg.UpstreamRepo,
		"upstream_tag":         gw.cfg.UpstreamTag,
		"upstream_digest":      upstream.Digest,
		"upstream_version":     upstream.Version,
		"known_server_version": knownSrvVer,
		"last_client_version":  lastCliVer,
		"upgrade_count":        gw.upgradeCount.Load(),
		"last_upgrade_reason":  lastReason,
		"last_update_error":    lastErr,
	}
	if !lastCheck.IsZero() {
		autoUpdate["last_update_check"] = lastCheck.UTC().Format(time.RFC3339)
	}
	if !lastUpgrade.IsZero() {
		autoUpdate["last_upgrade_time"] = lastUpgrade.UTC().Format(time.RFC3339)
	}

	gw.scaleMu.Lock()
	wpStatus := gw.workerPoolStatus
	lastScaleCheck := gw.lastScaleCheck
	lastScaleTime := gw.lastScaleTime
	lastScaleReason := gw.lastScaleReason
	gw.scaleMu.Unlock()

	workerPoolMap := map[string]any{
		"namespace":         gw.cfg.WorkerPoolNamespace,
		"name":              gw.cfg.WorkerPoolName,
		"spec_replicas":     wpStatus.SpecReplicas,
		"ready_replicas":    wpStatus.ReadyReplicas,
		"idle_timeout":      gw.cfg.WorkerPoolIdleTimeout.String(),
		"resume_timeout":    gw.cfg.ResumeTimeout.String(),
		"scale_up_count":    gw.scaleUpCount.Load(),
		"scale_down_count":  gw.scaleDownCount.Load(),
		"last_scale_reason": lastScaleReason,
	}
	if !lastScaleCheck.IsZero() {
		workerPoolMap["last_scale_check"] = lastScaleCheck.UTC().Format(time.RFC3339)
	}
	if !lastScaleTime.IsZero() {
		workerPoolMap["last_scale_time"] = lastScaleTime.UTC().Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"idle_timeout":      gw.cfg.IdleTimeout.String(),
		"max_active_worlds": gw.cfg.MaxActiveWorlds,
		"auto_update":       autoUpdate,
		"worker_pool":       workerPoolMap,
		"worlds":            worlds,
	})
}

func (gw *Gateway) handleWorldStatus(w http.ResponseWriter, r *http.Request) {
	gw.ensureWorkerPoolScaledUp(r.Context(), "world-status-http")

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

func (gw *Gateway) handleManualScale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if gw.scaler == nil {
		http.Error(w, "WorkerPool scaler not configured", http.StatusServiceUnavailable)
		return
	}
	repStr := r.URL.Query().Get("replicas")
	if repStr == "" {
		http.Error(w, "missing 'replicas' query parameter", http.StatusBadRequest)
		return
	}
	rep, err := strconv.Atoi(repStr)
	if err != nil || rep < 0 || rep > 10 {
		http.Error(w, "invalid replicas (must be integer between 0 and 10)", http.StatusBadRequest)
		return
	}

	gw.scaleMu.Lock()
	defer gw.scaleMu.Unlock()

	updated, err := gw.scaler.SetWorkerPoolReplicas(r.Context(), gw.cfg.WorkerPoolNamespace, gw.cfg.WorkerPoolName, int32(rep))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if int32(rep) > gw.workerPoolStatus.SpecReplicas {
		gw.scaleUpCount.Add(1)
	} else if int32(rep) < gw.workerPoolStatus.SpecReplicas {
		gw.scaleDownCount.Add(1)
	}
	gw.workerPoolStatus = updated
	gw.lastScaleTime = time.Now()
	gw.lastScaleReason = "manual-http-scale"

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"worker_pool": updated,
	})
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
		routesRaw         string
		udpBindIP         string
		httpListenAddr    string
		routerHTTPAddr    string
		idleTimeout       time.Duration
		wsRotateInterval  time.Duration
		preemptIdleGrace  time.Duration
		maxActiveWorlds   int
		templateAtespace  string
		templateName      string
		upstreamRepo      string
		upstreamTag       string
		imagePollInterval time.Duration
		ateapiAddr        string
		ateapiCAFile          string
		ateapiClientCert      string
		ateapiServerName      string
		workerpoolNamespace   string
		workerpoolName        string
		workerpoolIdleTimeout time.Duration
		resumeTimeout         time.Duration
	)
	flag.StringVar(&routesRaw, "routes", "34197=factorio/world-vanilla,34198=factorio/world-deathworld", "Comma-separated <udpPort>=<atespace>/<actor> routes")
	flag.StringVar(&udpBindIP, "udp-bind-ip", "0.0.0.0", "IP address to bind UDP listeners on")
	flag.StringVar(&httpListenAddr, "http-listen-addr", ":8080", "HTTP status/health listen address")
	flag.StringVar(&routerHTTPAddr, "router-http-addr", "atenet-router.ate-system.svc.cluster.local:80", "Substrate atenet-router HTTP address")
	flag.DurationVar(&idleTimeout, "idle-timeout", 10*time.Minute, "Duration of zero Factorio UDP traffic before suspending the Actor")
	flag.DurationVar(&wsRotateInterval, "ws-rotate-interval", 4*time.Minute, "WebSocket rotation interval (must be < Envoy 5m route-timeout)")
	flag.DurationVar(&preemptIdleGrace, "preempt-idle-grace", 5*time.Second, "Minimum idle time before a world can be preempted when multiplexing a full WorkerPool")
	flag.IntVar(&maxActiveWorlds, "max-active-worlds", 1, "Maximum concurrent running worlds (matches WorkerPool replicas)")
	flag.StringVar(&templateAtespace, "template-atespace", "factorio", "Atespace of the Factorio ActorTemplate to auto-upgrade")
	flag.StringVar(&templateName, "template-name", "factorio-server", "Name of the Factorio ActorTemplate to auto-upgrade")
	flag.StringVar(&upstreamRepo, "upstream-repo", "factoriotools/factorio", "Docker Hub repository to watch for new Factorio server releases")
	flag.StringVar(&upstreamTag, "upstream-tag", "stable", "Docker Hub tag to track for automatic Factorio ActorTemplate upgrades")
	flag.DurationVar(&imagePollInterval, "image-poll-interval", 30*time.Minute, "Background poll interval for checking Docker Hub when worlds are SUSPENDED (0 to disable)")
	flag.StringVar(&ateapiAddr, "ateapi-address", "api.ate-system.svc.cluster.local:443", "Substrate Control Plane gRPC address")
	flag.StringVar(&ateapiCAFile, "ateapi-ca-file", "/run/servicedns-ca/trust-bundle.pem", "CA bundle for verifying ate-api-server")
	flag.StringVar(&ateapiClientCert, "ateapi-client-cert", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "Pod Identity client cert bundle for mTLS to ate-api-server")
	flag.StringVar(&ateapiServerName, "ateapi-server-name", "api.ate-system.svc", "TLS ServerName for ate-api-server")
	flag.StringVar(&workerpoolNamespace, "workerpool-namespace", "factorio", "Namespace of the WorkerPool to scale on demand")
	flag.StringVar(&workerpoolName, "workerpool-name", "factorio", "Name of the WorkerPool to scale on demand")
	flag.DurationVar(&workerpoolIdleTimeout, "workerpool-idle-timeout", 2*time.Minute, "Duration all Factorio worlds must be suspended before scaling WorkerPool to 0")
	flag.DurationVar(&resumeTimeout, "resume-timeout", 5*time.Minute, "Maximum time allowed to resume an Actor and establish WebSocket tunnel (covers cold node boot)")
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

	scaler, err := newInClusterWorkerPoolScaler()
	if err != nil {
		slog.Warn("Failed to initialize in-cluster WorkerPool scaler; autoscaling disabled", "err", err)
	}

	gw := NewGateway(GatewayConfig{
		UDPBindIP:             udpBindIP,
		HTTPListenAddr:        httpListenAddr,
		RouterHTTPAddr:        routerHTTPAddr,
		Routes:                routes,
		IdleTimeout:           idleTimeout,
		WSRotateInterval:      wsRotateInterval,
		PreemptIdleGrace:      preemptIdleGrace,
		MaxActiveWorlds:       maxActiveWorlds,
		RequireFactorioPkt:    false,
		TemplateAtespace:      templateAtespace,
		TemplateName:          templateName,
		UpstreamRepo:          upstreamRepo,
		UpstreamTag:           upstreamTag,
		ImagePollInterval:     imagePollInterval,
		WorkerPoolNamespace:   workerpoolNamespace,
		WorkerPoolName:        workerpoolName,
		WorkerPoolIdleTimeout: workerpoolIdleTimeout,
		ResumeTimeout:         resumeTimeout,
		Scaler:                scaler,
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
