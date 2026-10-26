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

// Command factorio-probe sends real Factorio 2.x UDP ConnectionRequest
// packets to a factorio-gateway UDP endpoint, waiting for the target
// Factorio Actor to wake from GCS and reply with a Factorio
// ConnectionRequestReply packet.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/arbrown/factorio-substrate/internal/tunnelproto"
)

func main() {
	var (
		addr          string
		timeout       time.Duration
		retryInterval time.Duration
		keepAliveFor  time.Duration
	)
	flag.StringVar(&addr, "addr", "127.0.0.1:34197", "Factorio gateway UDP address (host:port)")
	flag.DurationVar(&timeout, "timeout", 60*time.Second, "Overall timeout waiting for Factorio UDP reply")
	flag.DurationVar(&retryInterval, "retry-interval", 500*time.Millisecond, "UDP handshake retry interval while Actor wakes")
	flag.DurationVar(&keepAliveFor, "keep-alive", 0, "Optional duration to continue sending UDP probes after first reply")
	flag.Parse()

	raddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		slog.Error("Failed to resolve UDP address", "addr", addr, "err", err)
		os.Exit(1)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		slog.Error("Failed to dial UDP", "addr", addr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	start := time.Now()
	deadline := start.Add(timeout)
	attempt := 0
	buf := make([]byte, 2048)

	slog.Info("Sending Factorio UDP ConnectionRequest probe", "target", addr, "timeout", timeout)
	for time.Now().Before(deadline) {
		attempt++
		pkt := tunnelproto.BuildFactorioProbePacket(uint32(attempt))
		if _, err := conn.Write(pkt); err != nil {
			slog.Warn("UDP write failed", "attempt", attempt, "err", err)
		}

		_ = conn.SetReadDeadline(time.Now().Add(retryInterval))
		n, err := conn.Read(buf)
		if err != nil {
			continue
		}
		reply := buf[:n]
		elapsed := time.Since(start)
		if !tunnelproto.IsFactorioServerReply(reply) {
			slog.Warn("Received non-Factorio UDP packet", "hex", hex.EncodeToString(reply))
			continue
		}

		version := "2.x"
		if len(reply) >= 8 {
			version = fmt.Sprintf("%d.%d.%d", reply[5], reply[6], reply[7])
		}
		slog.Info("Received genuine Factorio UDP ConnectionRequestReply!",
			"target", addr,
			"attempt", attempt,
			"wake_latency", elapsed.Round(time.Millisecond),
			"factorio_version", version,
			"reply_hex", hex.EncodeToString(reply),
		)

		if keepAliveFor > 0 {
			slog.Info("Maintaining simulated player UDP session", "duration", keepAliveFor)
			kaEnd := time.Now().Add(keepAliveFor)
			for time.Now().Before(kaEnd) {
				time.Sleep(1 * time.Second)
				_, _ = conn.Write(tunnelproto.BuildFactorioProbePacket(uint32(time.Now().Unix())))
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				_, _ = conn.Read(buf)
			}
		}
		return
	}

	slog.Error("Timed out waiting for Factorio UDP reply", "target", addr, "attempts", attempt, "elapsed", time.Since(start).Round(time.Millisecond))
	os.Exit(1)
}
