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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arbrown/factorio-substrate/internal/tunnelproto"
	"github.com/gorilla/websocket"
)

func TestSidecarWebSocketUDPBridgeAndReadyz(t *testing.T) {
	// Start a mock Factorio UDP server on 127.0.0.1:0.
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer udpConn.Close()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			// Reply with a valid Factorio ConnectionRequestReply header + echoed bytes.
			reply := append([]byte{0xd2, 0x02, 0x80, 0x00, 0x01, 0x02, 0x01, 0x17, 0x00}, buf[:n]...)
			_, _ = udpConn.WriteToUDP(reply, addr)
		}
	}()

	tmpDir := t.TempDir()
	sysDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(sysDir, "actor-name"), []byte("world-vanilla"), 0o644)
	_ = os.WriteFile(filepath.Join(sysDir, "atespace"), []byte("factorio"), 0o644)

	srv := NewSidecarServer(Config{
		FactorioUDPAddr:    udpConn.LocalAddr().String(),
		FactorioRCONAddr:   "127.0.0.1:1",
		FactorioDir:        tmpDir,
		SystemInfoDir:      sysDir,
		RCONPassword:       "testpw",
		SessionIdleTimeout: 5 * time.Second,
	})
	if err := srv.InitFactorioDir(); err != nil {
		t.Fatalf("InitFactorioDir failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "config", ".initialized")); err != nil {
		t.Fatalf("expected .initialized marker: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Verify /readyz returns 200 OK when the mock Factorio UDP listener responds.
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want 200", resp.StatusCode)
	}

	// 2. Connect via WebSocket and send a framed UDP packet.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	clientAddr := "203.0.113.7:54321"
	payload := tunnelproto.BuildFactorioProbePacket(42)
	frame, err := tunnelproto.EncodeFrame(clientAddr, payload)
	if err != nil {
		t.Fatalf("EncodeFrame failed: %v", err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, replyFrame, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	gotAddr, gotReply, err := tunnelproto.DecodeFrame(replyFrame)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if gotAddr != clientAddr {
		t.Errorf("gotAddr = %q, want %q", gotAddr, clientAddr)
	}
	if !bytes.HasSuffix(gotReply, payload) {
		t.Errorf("expected reply to contain echoed payload, got %x", gotReply)
	}
}

func sendControlCmd(t *testing.T, addr, cmd string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial control %s failed: %v", addr, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		t.Fatalf("Write control %q failed: %v", cmd, err)
	}
	b, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("Read control %q failed: %v", cmd, err)
	}
	return string(b)
}

func TestControlListenerAndFirstConnectWorldInit(t *testing.T) {
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer udpConn.Close()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			reply := append([]byte{0xd2, 0x02, 0x80, 0x00, 0x01, 0x02, 0x01, 0x18, 0x00}, buf[:n]...)
			_, _ = udpConn.WriteToUDP(reply, addr)
		}
	}()

	sysDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(sysDir, "actor-name"), []byte("world-ribbonworld"), 0o644)
	_ = os.WriteFile(filepath.Join(sysDir, "atespace"), []byte("factorio"), 0o644)

	srv := NewSidecarServer(Config{
		ControlAddr:        "127.0.0.1:0",
		FactorioUDPAddr:    udpConn.LocalAddr().String(),
		FactorioRCONAddr:   "127.0.0.1:1",
		SystemInfoDir:      sysDir,
		RCONPassword:       "testpw",
		SessionIdleTimeout: 5 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrlAddr, err := srv.StartControlListener(ctx)
	if err != nil {
		t.Fatalf("StartControlListener failed: %v", err)
	}
	cAddr := ctrlAddr.String()

	// 1. Before /ws is called (simulating ate-golden), actor-name must return "golden"
	// without reading /run/ate/actor-name.
	if got := strings.TrimSpace(sendControlCmd(t, cAddr, "actor-name")); got != "golden" {
		t.Fatalf("pre-activation actor-name = %q, want %q", got, "golden")
	}
	sendControlCmd(t, cAddr, "ready-existing:golden")

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 2. Connect via WebSocket (simulating first player connect after RestoreWorkload).
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Send two retry ConnectionRequest frames while the new map is being generated.
	clientAddr := "198.51.100.9:34197"
	for _, id := range []uint32{1, 2} {
		payload := tunnelproto.BuildFactorioProbePacket(id)
		frame, _ := tunnelproto.EncodeFrame(clientAddr, payload)
		if err := ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatalf("WriteMessage %d failed: %v", id, err)
		}
	}

	// Wait briefly for handleWebSocket to activate and buffer frames.
	time.Sleep(100 * time.Millisecond)

	// 3. Now control commands must reflect "world-ribbonworld".
	if got := strings.TrimSpace(sendControlCmd(t, cAddr, "actor-name")); got != "world-ribbonworld" {
		t.Fatalf("post-activation actor-name = %q, want %q", got, "world-ribbonworld")
	}
	if got := strings.TrimSpace(sendControlCmd(t, cAddr, "preset")); got != "ribbon-world" {
		t.Fatalf("preset = %q, want %q", got, "ribbon-world")
	}
	mapGenRaw := sendControlCmd(t, cAddr, "map-gen-settings.json")
	var mapGen map[string]any
	if err := json.Unmarshal([]byte(mapGenRaw), &mapGen); err != nil {
		t.Fatalf("unmarshal map-gen-settings.json: %v", err)
	}
	if mapGen["height"] != float64(64) {
		t.Fatalf("mapGen[height] = %v, want 64", mapGen["height"])
	}

	// 4. Signal that the factorio container finished generating world-ribbonworld.
	sendControlCmd(t, cAddr, "ready-existing:world-ribbonworld")

	// 5. Verify the coalesced latest ConnectionRequest frame (id=2) was delivered and replied to.
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, replyFrame, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage after world ready failed: %v", err)
	}
	gotAddr, gotReply, err := tunnelproto.DecodeFrame(replyFrame)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if gotAddr != clientAddr {
		t.Errorf("gotAddr = %q, want %q", gotAddr, clientAddr)
	}
	wantSuffix := tunnelproto.BuildFactorioProbePacket(2)
	if !bytes.HasSuffix(gotReply, wantSuffix) {
		t.Errorf("expected coalesced reply for packet id=2 (%x), got %x", wantSuffix, gotReply)
	}
}

func TestGetWorldSpecs(t *testing.T) {
	worlds := []struct {
		actor  string
		preset string
	}{
		{"world-vanilla", "rich-resources"},
		{"world-deathworld", "death-world"},
		{"world-railworld", "rail-world"},
		{"world-ribbonworld", "ribbon-world"},
	}
	seenNames := make(map[string]bool)
	for _, tc := range worlds {
		spec := GetWorldSpec(tc.actor)
		if spec.MapGenPreset != tc.preset {
			t.Errorf("GetWorldSpec(%q).MapGenPreset = %q, want %q", tc.actor, spec.MapGenPreset, tc.preset)
		}
		if seenNames[spec.ServerName] {
			t.Errorf("duplicate ServerName %q", spec.ServerName)
		}
		seenNames[spec.ServerName] = true
		if _, err := spec.ServerSettingsJSON(false); err != nil {
			t.Errorf("ServerSettingsJSON(%q) failed: %v", tc.actor, err)
		}
		if _, err := spec.MapGenSettingsJSON(); err != nil {
			t.Errorf("MapGenSettingsJSON(%q) failed: %v", tc.actor, err)
		}
	}
}
