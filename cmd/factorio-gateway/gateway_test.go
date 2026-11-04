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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arbrown/factorio-substrate/internal/tunnelproto"
	"github.com/gorilla/websocket"
)

type fakeSuspender struct {
	mu           sync.Mutex
	suspended    []string
	tmplInfo     TemplateSnapshotInfo
	upgradeCalls []string
}

func (f *fakeSuspender) SuspendActor(_ context.Context, atespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, atespace+"/"+name)
	return nil
}

func (f *fakeSuspender) GetTemplateInfo(_ context.Context, _, _ string) (TemplateSnapshotInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tmplInfo, nil
}

func (f *fakeSuspender) UpgradeTemplateImage(_ context.Context, _, _, newImageRef string) (TemplateSnapshotInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgradeCalls = append(f.upgradeCalls, newImageRef)
	f.tmplInfo = TemplateSnapshotInfo{
		UID:         fmt.Sprintf("uid-%d", len(f.upgradeCalls)+1),
		ImageRef:    newImageRef,
		SnapshotURI: fmt.Sprintf("gs://bucket/golden-%d", len(f.upgradeCalls)+1),
	}
	return f.tmplInfo, nil
}

func (f *fakeSuspender) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.suspended...)
}

func (f *fakeSuspender) upgrades() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.upgradeCalls...)
}

type fakeRegistry struct {
	mu      sync.Mutex
	release UpstreamRelease
}

func (r *fakeRegistry) set(version, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release = UpstreamRelease{
		ImageRef: "index.docker.io/factoriotools/factorio@" + digest,
		Digest:   digest,
		Version:  version,
	}
}

func (r *fakeRegistry) ResolveLatestRelease(_ context.Context, _, _ string) (UpstreamRelease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.release, nil
}

func TestGatewayWakeMultiplexAndIdleSuspend(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	// Mock atenet-router that echoes framed UDP packets back as Factorio replies.
	routerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := r.Header.Get(targetActorHeader)
		if actor == "" {
			http.Error(w, "missing ate-target-actor", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/prepare-suspend" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"save_output":"Saved"}`))
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			mt, frame, err := ws.ReadMessage()
			if err != nil {
				return
			}
			clientAddr, payload, err := tunnelproto.DecodeFrame(frame)
			if err != nil {
				continue
			}
			replyPayload := append([]byte(actor+":"), payload...)
			replyFrame, _ := tunnelproto.EncodeFrame(clientAddr, replyPayload)
			_ = ws.WriteMessage(mt, replyFrame)
		}
	}))
	defer routerSrv.Close()

	routerHost := strings.TrimPrefix(routerSrv.URL, "http://")
	suspender := &fakeSuspender{}

	gw := NewGateway(GatewayConfig{
		UDPBindIP:          "127.0.0.1",
		RouterHTTPAddr:     routerHost,
		IdleTimeout:        1200 * time.Millisecond,
		WSRotateInterval:   10 * time.Second,
		PreemptIdleGrace:   50 * time.Millisecond,
		MaxActiveWorlds:    1,
		RequireFactorioPkt: false,
		Routes: []RouteSpec{
			{UDPPort: 0, Atespace: "factorio", Actor: "world-vanilla"},
			{UDPPort: 0, Atespace: "factorio", Actor: "world-deathworld"},
		},
	}, suspender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := gw.Start(ctx); err != nil {
		t.Fatalf("gw.Start failed: %v", err)
	}
	defer gw.Close()

	vanillaWT := gw.worlds["factorio/world-vanilla"]
	deathWT := gw.worlds["factorio/world-deathworld"]

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer clientConn.Close()

	// 1. Send a 1-byte scanner packet to world-vanilla; verify it is dropped and does not wake the world.
	_, _ = clientConn.WriteToUDP([]byte("?"), vanillaWT.LocalUDPAddr().(*net.UDPAddr))
	time.Sleep(50 * time.Millisecond)
	if vanillaWT.scannerDropped.Load() != 1 {
		t.Fatalf("expected 1 dropped scanner packet, got %d", vanillaWT.scannerDropped.Load())
	}

	// 2. Send a valid Factorio probe packet to world-vanilla; verify it wakes and replies.
	probe := tunnelproto.BuildFactorioProbePacket(101)
	_, _ = clientConn.WriteToUDP(probe, vanillaWT.LocalUDPAddr().(*net.UDPAddr))

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP for world-vanilla failed: %v", err)
	}
	if !bytes.HasPrefix(buf[:n], []byte("factorio/world-vanilla:")) {
		t.Fatalf("unexpected reply from world-vanilla: %q", string(buf[:n]))
	}

	// 3. Wait PreemptIdleGrace (60ms) and connect to world-deathworld; verify world-vanilla is preempted/suspended!
	time.Sleep(80 * time.Millisecond)
	_, _ = clientConn.WriteToUDP(probe, deathWT.LocalUDPAddr().(*net.UDPAddr))

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err = clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP for world-deathworld failed: %v", err)
	}
	if !bytes.HasPrefix(buf[:n], []byte("factorio/world-deathworld:")) {
		t.Fatalf("unexpected reply from world-deathworld: %q", string(buf[:n]))
	}

	suspended := suspender.list()
	if len(suspended) != 1 || suspended[0] != "factorio/world-vanilla" {
		t.Fatalf("expected world-vanilla to be preempt-suspended, got %v", suspended)
	}

	// 4. Wait for IdleTimeout (1.2s) and verify world-deathworld auto-suspends!
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list := suspender.list()
		if len(list) >= 2 && list[1] == "factorio/world-deathworld" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("expected world-deathworld to auto-suspend after idle timeout, got %v", suspender.list())
}

func TestGatewayProactiveAndReactiveAutoUpgrade(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	routerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prepare-suspend" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"save_output":"Saved"}`))
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			mt, frame, err := ws.ReadMessage()
			if err != nil {
				return
			}
			clientAddr, payload, err := tunnelproto.DecodeFrame(frame)
			if err != nil {
				continue
			}
			replyFrame, _ := tunnelproto.EncodeFrame(clientAddr, payload)
			_ = ws.WriteMessage(mt, replyFrame)
		}
	}))
	defer routerSrv.Close()

	routerHost := strings.TrimPrefix(routerSrv.URL, "http://")
	suspender := &fakeSuspender{
		tmplInfo: TemplateSnapshotInfo{
			UID:         "uid-1",
			ImageRef:    "index.docker.io/factoriotools/factorio@sha256:old2123",
			SnapshotURI: "gs://bucket/golden-1",
		},
	}
	reg := &fakeRegistry{}
	reg.set("2.1.23", "sha256:old2123")

	gw := NewGateway(GatewayConfig{
		UDPBindIP:         "127.0.0.1",
		RouterHTTPAddr:    routerHost,
		IdleTimeout:       10 * time.Second,
		WSRotateInterval:  10 * time.Second,
		PreemptIdleGrace:  50 * time.Millisecond,
		MaxActiveWorlds:   1,
		ImagePollInterval: 100 * time.Millisecond,
		Registry:          reg,
		Routes: []RouteSpec{
			{UDPPort: 0, Atespace: "factorio", Actor: "world-vanilla"},
		},
	}, suspender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := gw.Start(ctx); err != nil {
		t.Fatalf("gw.Start failed: %v", err)
	}
	defer gw.Close()

	// 1. Proactive background watcher: advance upstream registry to 2.1.24 (sha256:new2124)
	// while world-vanilla is SUSPENDED, and verify the background poll upgrades ActorTemplate.
	reg.set("2.1.24", "sha256:new2124")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ups := suspender.upgrades(); len(ups) >= 1 {
			if ups[0] != "index.docker.io/factoriotools/factorio@sha256:new2124" {
				t.Fatalf("unexpected proactive upgrade image: %q", ups[0])
			}
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if len(suspender.upgrades()) < 1 {
		t.Fatalf("expected proactive background poll to upgrade ActorTemplate to 2.1.24")
	}

	// 2. Reactive client version sniffer: pause background polling by setting a long interval,
	// advance upstream registry to 2.1.25 (sha256:new2125), and send a UDP ConnectionRequest
	// advertising client version 2.1.25. Verify the gateway upgrades ActorTemplate BEFORE waking!
	gw.cfg.ImagePollInterval = 1 * time.Hour
	reg.set("2.1.25", "sha256:new2125")

	vanillaWT := gw.worlds["factorio/world-vanilla"]
	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer clientConn.Close()

	probe25 := tunnelproto.BuildFactorioProbePacketForVersion(777, 2, 1, 25)
	_, _ = clientConn.WriteToUDP(probe25, vanillaWT.LocalUDPAddr().(*net.UDPAddr))

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	if _, _, err := clientConn.ReadFromUDP(buf); err != nil {
		t.Fatalf("ReadFromUDP after reactive upgrade failed: %v", err)
	}

	ups := suspender.upgrades()
	if len(ups) != 2 || ups[1] != "index.docker.io/factoriotools/factorio@sha256:new2125" {
		t.Fatalf("expected reactive client version sniffer to upgrade ActorTemplate to 2.1.25, got %v", ups)
	}

	// Verify /status reports the auto_update metadata.
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	gw.HTTPHandler().ServeHTTP(rec, req)
	var statusPayload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("unmarshal /status failed: %v", err)
	}
	autoUpd, _ := statusPayload["auto_update"].(map[string]any)
	if got := autoUpd["known_server_version"]; got != "2.1.25" {
		t.Fatalf("expected known_server_version=2.1.25 in /status, got %v", got)
	}
	if got := autoUpd["last_client_version"]; got != "2.1.25" {
		t.Fatalf("expected last_client_version=2.1.25 in /status, got %v", got)
	}
}

type fakeWorkerPoolScaler struct {
	mu         sync.Mutex
	status     WorkerPoolStatus
	scaleCalls []int32
}

func (s *fakeWorkerPoolScaler) GetWorkerPoolStatus(_ context.Context, _, _ string) (WorkerPoolStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, nil
}

func (s *fakeWorkerPoolScaler) SetWorkerPoolReplicas(_ context.Context, _, _ string, replicas int32) (WorkerPoolStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scaleCalls = append(s.scaleCalls, replicas)
	s.status.SpecReplicas = replicas
	s.status.ReadyReplicas = replicas
	return s.status, nil
}

func (s *fakeWorkerPoolScaler) calls() []int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int32(nil), s.scaleCalls...)
}

func TestGatewayWorkerPoolScaleToZeroAndColdWake(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	routerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prepare-suspend" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"save_output":"Saved"}`))
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			mt, frame, err := ws.ReadMessage()
			if err != nil {
				return
			}
			clientAddr, payload, err := tunnelproto.DecodeFrame(frame)
			if err != nil {
				continue
			}
			replyPayload := append([]byte("echo:"), payload...)
			replyFrame, _ := tunnelproto.EncodeFrame(clientAddr, replyPayload)
			_ = ws.WriteMessage(mt, replyFrame)
		}
	}))
	defer routerSrv.Close()

	routerHost := strings.TrimPrefix(routerSrv.URL, "http://")
	suspender := &fakeSuspender{
		tmplInfo: TemplateSnapshotInfo{
			UID:         "uid-test",
			ImageRef:    "factorio:latest",
			SnapshotURI: "gs://bucket/golden-test",
		},
	}
	scaler := &fakeWorkerPoolScaler{
		status: WorkerPoolStatus{SpecReplicas: 0, ReadyReplicas: 0},
	}

	gw := NewGateway(GatewayConfig{
		UDPBindIP:             "127.0.0.1",
		RouterHTTPAddr:        routerHost,
		IdleTimeout:           100 * time.Millisecond,
		WSRotateInterval:      10 * time.Second,
		PreemptIdleGrace:      20 * time.Millisecond,
		MaxActiveWorlds:       1,
		WorkerPoolNamespace:   "factorio",
		WorkerPoolName:        "factorio",
		WorkerPoolIdleTimeout: 150 * time.Millisecond,
		ResumeTimeout:         3 * time.Second,
		Scaler:                scaler,
		Routes: []RouteSpec{
			{UDPPort: 0, Atespace: "factorio", Actor: "world-vanilla"},
		},
	}, suspender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := gw.Start(ctx); err != nil {
		t.Fatalf("gw.Start failed: %v", err)
	}
	defer gw.Close()

	vanillaWT := gw.worlds["factorio/world-vanilla"]
	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer clientConn.Close()

	// 1. Send UDP probe packet while WorkerPool is at 0 replicas; verify it triggers 0 -> 1 scale-up!
	probe := tunnelproto.BuildFactorioProbePacket(123)
	_, _ = clientConn.WriteToUDP(probe, vanillaWT.LocalUDPAddr().(*net.UDPAddr))

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP failed: %v", err)
	}
	if !bytes.HasPrefix(buf[:n], []byte("echo:")) {
		t.Fatalf("unexpected reply: %q", string(buf[:n]))
	}

	calls := scaler.calls()
	if len(calls) == 0 || calls[0] != 1 {
		t.Fatalf("expected WorkerPool to scale up to 1 replica on UDP traffic, got calls: %v", calls)
	}

	// 2. Query /status endpoint; verify worker_pool is reported with spec_replicas=1.
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	gw.HTTPHandler().ServeHTTP(rec, req)
	var statusPayload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("unmarshal /status failed: %v", err)
	}
	wp, _ := statusPayload["worker_pool"].(map[string]any)
	if wp == nil {
		t.Fatalf("missing worker_pool in /status response: %v", statusPayload)
	}
	if got := wp["spec_replicas"]; got != float64(1) {
		t.Fatalf("expected spec_replicas=1 in /status, got %v", got)
	}
	if got := wp["scale_up_count"]; got != float64(1) {
		t.Fatalf("expected scale_up_count=1 in /status, got %v", got)
	}

	// 3. Wait for IdleTimeout (100ms) + WorkerPoolIdleTimeout (150ms).
	// idleReaperLoop should automatically scale WorkerPool from 1 -> 0!
	deadline := time.Now().Add(3 * time.Second)
	scaledDown := false
	for time.Now().Before(deadline) {
		calls = scaler.calls()
		if len(calls) >= 2 && calls[len(calls)-1] == 0 {
			scaledDown = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !scaledDown {
		t.Fatalf("expected idleReaperLoop to scale down WorkerPool to 0 after idle timeout, got calls: %v", scaler.calls())
	}

	// 4. Test /scale endpoint: manually scale up to 1, then scale down to 0.
	scaleUpReq := httptest.NewRequest(http.MethodPost, "/scale?replicas=1", nil)
	scaleUpRec := httptest.NewRecorder()
	gw.HTTPHandler().ServeHTTP(scaleUpRec, scaleUpReq)
	if scaleUpRec.Code != http.StatusOK {
		t.Fatalf("/scale?replicas=1 failed: code=%d body=%s", scaleUpRec.Code, scaleUpRec.Body.String())
	}
	calls = scaler.calls()
	if calls[len(calls)-1] != 1 {
		t.Fatalf("expected /scale?replicas=1 to scale up, got calls: %v", calls)
	}

	scaleDownReq := httptest.NewRequest(http.MethodPost, "/scale?replicas=0", nil)
	scaleDownRec := httptest.NewRecorder()
	gw.HTTPHandler().ServeHTTP(scaleDownRec, scaleDownReq)
	if scaleDownRec.Code != http.StatusOK {
		t.Fatalf("/scale?replicas=0 failed: code=%d body=%s", scaleDownRec.Code, scaleDownRec.Body.String())
	}
	calls = scaler.calls()
	if calls[len(calls)-1] != 0 {
		t.Fatalf("expected /scale?replicas=0 to scale down, got calls: %v", calls)
	}
}
