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

package tunnelproto

import (
	"bytes"
	"net"
	"testing"
)

func TestEncodeDecodeFrame(t *testing.T) {
	addr := "198.51.100.42:51234"
	payload := BuildFactorioProbePacket(0xdeadbeef)

	frame, err := EncodeFrame(addr, payload)
	if err != nil {
		t.Fatalf("EncodeFrame failed: %v", err)
	}

	gotAddr, gotPayload, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}
	if gotAddr != addr {
		t.Errorf("gotAddr = %q, want %q", gotAddr, addr)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("gotPayload = %x, want %x", gotPayload, payload)
	}
}

func TestEncodeDecodeFrameErrors(t *testing.T) {
	if _, err := EncodeFrame("", []byte{1, 2, 3}); err == nil {
		t.Error("expected error for empty clientAddr")
	}
	if _, _, err := DecodeFrame([]byte{0x00}); err == nil {
		t.Error("expected error for short frame")
	}
	if _, _, err := DecodeFrame([]byte{0x00, 0x10, 'a'}); err == nil {
		t.Error("expected error for truncated clientAddr")
	}
}

func TestFactorioPacketClassification(t *testing.T) {
	probe := BuildFactorioProbePacket(12345)
	if !IsLikelyFactorioClientPacket(probe) {
		t.Errorf("expected BuildFactorioProbePacket to be classified as Factorio client packet")
	}

	// Simulated server reply observed from Factorio 2.1.23: d20280000102011700
	reply := []byte{0xd2, 0x02, 0x80, 0x00, 0x01, 0x02, 0x01, 0x17, 0x00}
	if !IsFactorioServerReply(reply) {
		t.Errorf("expected Factorio ConnectionRequestReply (0xd2 & 0x1f == 0x12) to be recognized as server reply")
	}

	// Short scanner packet should be rejected.
	if IsLikelyFactorioClientPacket([]byte("hi")) {
		t.Errorf("expected 2-byte scanner packet to be rejected")
	}
}

func TestVersionParsingAndComparison(t *testing.T) {
	probe24 := BuildFactorioProbePacketForVersion(999, 2, 1, 24)
	if v, ok := ParseClientVersion(probe24); !ok || v != "2.1.24" {
		t.Fatalf("ParseClientVersion(probe24) = %q, %v; want 2.1.24, true", v, ok)
	}

	probe25 := BuildFactorioProbePacketForVersion(999, 2, 1, 25)
	if v, ok := ParseClientVersion(probe25); !ok || v != "2.1.25" {
		t.Fatalf("ParseClientVersion(probe25) = %q, %v; want 2.1.25, true", v, ok)
	}

	reply23 := []byte{0xd2, 0x02, 0x80, 0x00, 0x01, 0x02, 0x01, 0x17, 0x00}
	if v, ok := ParseServerReplyVersion(reply23); !ok || v != "2.1.23" {
		t.Fatalf("ParseServerReplyVersion(reply23) = %q, %v; want 2.1.23, true", v, ok)
	}

	reply24 := []byte{0xd2, 0x04, 0x80, 0x00, 0x01, 0x02, 0x01, 0x18, 0x00}
	if v, ok := ParseServerReplyVersion(reply24); !ok || v != "2.1.24" {
		t.Fatalf("ParseServerReplyVersion(reply24) = %q, %v; want 2.1.24, true", v, ok)
	}

	// Ensure a map download TransferBlock packet (msgType 0x0c) containing
	// arbitrary bytes like 02 00 00 00 or 02 01 14 00 is NEVER misparsed as a
	// ConnectionRequestReply version header!
	mapTransferBlock := []byte{0x0c, 0x00, 0x01, 0x02, 0x00, 0x02, 0x00, 0x00, 0x00, 0xaa, 0xbb}
	if v, ok := ParseServerReplyVersion(mapTransferBlock); ok {
		t.Fatalf("expected map TransferBlock to be ignored by ParseServerReplyVersion, got %q", v)
	}

	if cmp := CompareVersions("2.1.24", "2.1.23"); cmp != 1 {
		t.Errorf("CompareVersions(2.1.24, 2.1.23) = %d, want 1", cmp)
	}
	if cmp := CompareVersions("2.1.23", "2.1.24"); cmp != -1 {
		t.Errorf("CompareVersions(2.1.23, 2.1.24) = %d, want -1", cmp)
	}
	if cmp := CompareVersions("2.1.24", "2.1.24"); cmp != 0 {
		t.Errorf("CompareVersions(2.1.24, 2.1.24) = %d, want 0", cmp)
	}
}

func TestIsIgnoredClientAddr(t *testing.T) {
	tests := []struct {
		name string
		addr *net.UDPAddr
		want bool
	}{
		{name: "nil", addr: nil, want: true},
		{name: "zero port", addr: &net.UDPAddr{IP: net.IPv4(34, 125, 1, 90), Port: 0}, want: true},
		{name: "gce metadata dns", addr: &net.UDPAddr{IP: net.IPv4(169, 254, 169, 254), Port: 53}, want: true},
		{name: "gce metadata non-dns ipv4", addr: &net.UDPAddr{IP: net.IPv4(169, 254, 169, 254), Port: 45000}, want: true},
		{name: "gce metadata ipv4-mapped ipv6", addr: &net.UDPAddr{IP: net.ParseIP("::ffff:169.254.169.254"), Port: 45000}, want: true},
		{name: "link-local ipv4", addr: &net.UDPAddr{IP: net.IPv4(169, 254, 1, 2), Port: 34197}, want: true},
		{name: "link-local ipv6", addr: &net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 45000}, want: true},
		{name: "public dns reflection", addr: &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 53}, want: true},
		{name: "unspecified ipv4", addr: &net.UDPAddr{IP: net.IPv4zero, Port: 45000}, want: true},
		{name: "unspecified ipv6", addr: &net.UDPAddr{IP: net.IPv6zero, Port: 45000}, want: true},
		{name: "multicast ipv4", addr: &net.UDPAddr{IP: net.IPv4(224, 0, 0, 1), Port: 45000}, want: true},
		{name: "multicast ipv6", addr: &net.UDPAddr{IP: net.ParseIP("ff02::1"), Port: 45000}, want: true},
		{name: "loopback ipv4 allowed", addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 54321}, want: false},
		{name: "loopback ipv6 allowed", addr: &net.UDPAddr{IP: net.IPv6loopback, Port: 54321}, want: false},
		{name: "home client allowed", addr: &net.UDPAddr{IP: net.IPv4(136, 36, 95, 238), Port: 43969}, want: false},
		{name: "cloudtop probe allowed", addr: &net.UDPAddr{IP: net.IPv4(34, 125, 1, 90), Port: 12288}, want: false},
		{name: "vpc internal allowed", addr: &net.UDPAddr{IP: net.IPv4(10, 128, 0, 29), Port: 50000}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIgnoredClientAddr(tc.addr); got != tc.want {
				t.Errorf("IsIgnoredClientAddr(%v) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
