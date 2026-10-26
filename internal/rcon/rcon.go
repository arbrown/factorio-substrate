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

// Package rcon implements a lightweight Source RCON TCP client for executing
// administrative commands (such as /save, /players, /seed, /time) against the
// local Factorio headless server inside the Actor sandbox.
package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	packetTypeAuth        int32 = 3
	packetTypeAuthResp    int32 = 2
	packetTypeExecCommand int32 = 2
	packetTypeRespValue   int32 = 0

	maxPacketSize = 64 * 1024
)

// Execute dials the Factorio RCON TCP endpoint at addr, authenticates with
// password, runs command, and returns the response body.
func Execute(ctx context.Context, addr, password, command string) (string, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("rcon dial %s: %w", addr, err)
	}
	defer conn.Close()

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = conn.SetDeadline(deadline)

	const authReqID int32 = 1
	if err := writePacket(conn, authReqID, packetTypeAuth, password); err != nil {
		return "", fmt.Errorf("rcon write auth: %w", err)
	}

	// Factorio sends an empty RESPONSE_VALUE (or directly SERVERDATA_AUTH_RESPONSE)
	// followed by SERVERDATA_AUTH_RESPONSE with the matching request ID (or -1 on failure).
	for {
		respID, respType, _, err := readPacket(conn)
		if err != nil {
			return "", fmt.Errorf("rcon read auth response: %w", err)
		}
		if respType == packetTypeAuthResp {
			if respID == -1 || respID != authReqID {
				return "", errors.New("rcon authentication failed")
			}
			break
		}
	}

	const cmdReqID int32 = 2
	if err := writePacket(conn, cmdReqID, packetTypeExecCommand, command); err != nil {
		return "", fmt.Errorf("rcon write command: %w", err)
	}

	respID, _, body, err := readPacket(conn)
	if err != nil {
		return "", fmt.Errorf("rcon read command response: %w", err)
	}
	if respID != cmdReqID {
		return "", fmt.Errorf("rcon unexpected response ID %d (want %d)", respID, cmdReqID)
	}
	return body, nil
}

func writePacket(w io.Writer, reqID, pktType int32, body string) error {
	bodyBytes := []byte(body)
	// Size field is 4 (reqID) + 4 (pktType) + len(body) + 2 (two null terminators).
	size := int32(4 + 4 + len(bodyBytes) + 2)
	buf := make([]byte, 4+size)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(reqID))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(pktType))
	copy(buf[12:12+len(bodyBytes)], bodyBytes)
	// Trailing two bytes are already 0x00, 0x00.
	_, err := w.Write(buf)
	return err
}

func readPacket(r io.Reader) (reqID, pktType int32, body string, err error) {
	var sizeBuf [4]byte
	if _, err := io.ReadFull(r, sizeBuf[:]); err != nil {
		return 0, 0, "", err
	}
	size := int32(binary.LittleEndian.Uint32(sizeBuf[:]))
	if size < 10 || size > maxPacketSize {
		return 0, 0, "", fmt.Errorf("rcon invalid packet size %d", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, 0, "", err
	}
	reqID = int32(binary.LittleEndian.Uint32(payload[0:4]))
	pktType = int32(binary.LittleEndian.Uint32(payload[4:8]))
	body = string(payload[8 : size-2])
	return reqID, pktType, body, nil
}
