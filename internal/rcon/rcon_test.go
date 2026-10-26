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

package rcon

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestRCONExecute(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reqID, pktType, body, err := readPacket(conn)
		if err != nil || pktType != packetTypeAuth || body != "secret-pw" {
			_ = writePacket(conn, -1, packetTypeAuthResp, "")
			return
		}
		_ = writePacket(conn, reqID, packetTypeAuthResp, "")

		cmdID, cmdType, cmdBody, err := readPacket(conn)
		if err != nil || cmdType != packetTypeExecCommand {
			return
		}
		_ = writePacket(conn, cmdID, packetTypeRespValue, "executed:"+cmdBody)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := Execute(ctx, ln.Addr().String(), "secret-pw", "/save")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if out != "executed:/save" {
		t.Errorf("got %q, want %q", out, "executed:/save")
	}
}
