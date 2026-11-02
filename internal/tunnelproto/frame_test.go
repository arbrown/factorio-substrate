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
