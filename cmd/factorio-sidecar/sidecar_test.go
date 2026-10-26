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
