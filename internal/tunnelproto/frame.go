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

// Package tunnelproto defines the binary framing used to encapsulate UDP
// datagrams (preserving client IP:port) over WebSocket streams between the
// always-on UDP gateway pod and the in-actor sidecar, plus helpers for
// inspecting Factorio's UDP multiplayer protocol.
package tunnelproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// MaxAddrLen bounds the client address string length in a frame.
	MaxAddrLen = 256
	// MaxPayloadLen bounds a single UDP datagram payload.
	MaxPayloadLen = 65535

	// FactorioMessageTypeMask extracts the 5-bit message type from byte 0 of a
	// Factorio UDP packet.
	FactorioMessageTypeMask = 0x1f

	// FactorioMsgConnectionRequest is sent by a Factorio client to initiate a
	// multiplayer connection handshake.
	FactorioMsgConnectionRequest = 0x02
	// FactorioMsgConnectionRequestReply is sent by the Factorio server in
	// response to a ConnectionRequest.
	FactorioMsgConnectionRequestReply = 0x03
	// FactorioMsgConnectionRequestReplyConfirm is the second client handshake packet.
	FactorioMsgConnectionRequestReplyConfirm = 0x04
	// FactorioMsgConnectionAcceptOrDeny is the second server handshake packet.
	FactorioMsgConnectionAcceptOrDeny = 0x05
)

var (
	ErrFrameTooShort   = errors.New("tunnelproto: frame too short")
	ErrInvalidAddrLen  = errors.New("tunnelproto: invalid client address length")
	ErrPayloadTooLarge = errors.New("tunnelproto: payload exceeds maximum UDP size")
)

// EncodeFrame serializes a client UDP address (e.g. "203.0.113.10:54123") and
// raw UDP datagram payload into a single binary WebSocket frame:
//
//	[2-byte big-endian len(clientAddr)][clientAddr bytes][payload bytes]
func EncodeFrame(clientAddr string, payload []byte) ([]byte, error) {
	addrLen := len(clientAddr)
	if addrLen == 0 || addrLen > MaxAddrLen {
		return nil, fmt.Errorf("%w: %d", ErrInvalidAddrLen, addrLen)
	}
	if len(payload) > MaxPayloadLen {
		return nil, fmt.Errorf("%w: %d", ErrPayloadTooLarge, len(payload))
	}
	buf := make([]byte, 2+addrLen+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(addrLen))
	copy(buf[2:2+addrLen], clientAddr)
	copy(buf[2+addrLen:], payload)
	return buf, nil
}

// DecodeFrame extracts the client UDP address and raw UDP payload from a
// binary WebSocket frame encoded by EncodeFrame.
func DecodeFrame(frame []byte) (clientAddr string, payload []byte, err error) {
	if len(frame) < 3 {
		return "", nil, ErrFrameTooShort
	}
	addrLen := int(binary.BigEndian.Uint16(frame[:2]))
	if addrLen == 0 || addrLen > MaxAddrLen || 2+addrLen > len(frame) {
		return "", nil, fmt.Errorf("%w: %d (frame len %d)", ErrInvalidAddrLen, addrLen, len(frame))
	}
	clientAddr = string(frame[2 : 2+addrLen])
	payload = frame[2+addrLen:]
	return clientAddr, payload, nil
}

// BuildFactorioProbePacket builds a Factorio 2.x UDP ConnectionRequest packet
// that causes a running Factorio headless server to reply with a
// ConnectionRequestReply (message type 0x03) without completing a login.
func BuildFactorioProbePacket(requestID uint32) []byte {
	return BuildFactorioProbePacketForVersion(requestID, 2, 1, 24)
}

// BuildFactorioProbePacketForVersion builds a Factorio UDP ConnectionRequest
// packet advertising a specific major.minor.patch client version.
func BuildFactorioProbePacketForVersion(requestID uint32, major, minor, patch uint8) []byte {
	pkt := []byte{
		0x42,                       // Type 0x02 (ConnectionRequest) + reliability flags
		major, minor, patch, 0x00, // Factorio major.minor.patch.0 version header
		0x56, 0x56, // Protocol magic / build marker
		0x00, 0x00, 0x00, 0x00, // Client request ID (filled below)
		0x55, 0x66, 0x77, 0x88,
	}
	binary.LittleEndian.PutUint32(pkt[7:11], requestID)
	return pkt
}

// ParseClientVersion extracts the 3-component Factorio version ("2.x.y") from
// an inbound client UDP ConnectionRequest (0x02) packet, if present.
func ParseClientVersion(pkt []byte) (string, bool) {
	if len(pkt) < 5 || (pkt[0]&FactorioMessageTypeMask) != FactorioMsgConnectionRequest {
		return "", false
	}
	return scanFactorio2VersionBytes(pkt[1:])
}

// ParseServerReplyVersion extracts the 3-component Factorio version ("2.x.y")
// from an outbound server ConnectionRequestReply packet (e.g. d2 04 80 00 01 02 01 18 00),
// if present.
func ParseServerReplyVersion(pkt []byte) (string, bool) {
	if !IsFactorioServerReply(pkt) || len(pkt) < 9 {
		return "", false
	}
	// In Factorio 2.x ConnectionRequestReply packets, the 4-byte
	// ApplicationVersion [2, minor, patch, 0] sits at offset 5 (pkt[5:9]).
	if len(pkt) >= 9 && isFactorio2VersionQuad(pkt[5:9]) {
		return fmt.Sprintf("%d.%d.%d", pkt[5], pkt[6], pkt[7]), true
	}
	return scanFactorio2VersionBytes(pkt[1:])
}

func scanFactorio2VersionBytes(b []byte) (string, bool) {
	limit := len(b) - 4
	if limit > 12 {
		limit = 12
	}
	for i := 0; i <= limit; i++ {
		if isFactorio2VersionQuad(b[i : i+4]) {
			return fmt.Sprintf("%d.%d.%d", b[i], b[i+1], b[i+2]), true
		}
	}
	return "", false
}

func isFactorio2VersionQuad(q []byte) bool {
	return len(q) == 4 && q[0] == 2 && q[1] <= 20 && q[2] <= 250 && q[3] == 0
}

// CompareVersions compares two dotted numeric version strings (e.g. "2.1.24" vs "2.1.23").
// It returns -1 if a < b, 0 if a == b, and +1 if a > b.
func CompareVersions(a, b string) int {
	pa, okA := parseVersionParts(a)
	pb, okB := parseVersionParts(b)
	if !okA || !okB {
		return strings.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

func parseVersionParts(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 3 {
		return out, false
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// IsFactorioServerReply returns true if pkt is a UDP packet sent by a Factorio
// server (such as ConnectionRequestReply 0x03 or ConnectionAcceptOrDeny 0x05
// or ServerHeartbeat/Transfer).
func IsFactorioServerReply(pkt []byte) bool {
	if len(pkt) < 4 {
		return false
	}
	msgType := pkt[0] & FactorioMessageTypeMask
	return msgType == FactorioMsgConnectionRequestReply ||
		msgType == FactorioMsgConnectionAcceptOrDeny ||
		msgType == 0x01 || // PingReply
		msgType >= 0x06
}

// IsLikelyFactorioClientPacket checks whether an inbound UDP datagram can
// plausibly be the start or continuation of a Factorio client session, as
// opposed to a 1-byte or ASCII port-scanner probe (e.g. Shodan/Censys).
func IsLikelyFactorioClientPacket(pkt []byte) bool {
	if len(pkt) < 5 {
		return false
	}
	msgType := pkt[0] & FactorioMessageTypeMask
	switch msgType {
	case FactorioMsgConnectionRequest,
		FactorioMsgConnectionRequestReplyConfirm,
		0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
		0x10, 0x11, 0x12, 0x13, 0x14:
		return true
	default:
		return false
	}
}
