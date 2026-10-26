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
	mu        sync.Mutex
	suspended []string
}

func (f *fakeSuspender) SuspendActor(_ context.Context, atespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, atespace+"/"+name)
	return nil
}

func (f *fakeSuspender) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.suspended...)
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
