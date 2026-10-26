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
	pkt := []byte{
		0x42,                   // Type 0x02 (ConnectionRequest) + reliability flags
		0x02, 0x01, 0x17, 0x00, // Factorio 2.1.23.0 version header
		0x56, 0x56, // Protocol magic / build marker
		0x00, 0x00, 0x00, 0x00, // Client request ID (filled below)
		0x55, 0x66, 0x77, 0x88,
	}
	binary.LittleEndian.PutUint32(pkt[7:11], requestID)
	return pkt
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
