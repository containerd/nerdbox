/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package ttrpcutil

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// PingTTRPC sends an invalid request to a TTRPC server to check
// for a response, indicating the server is alive and accepting
// requests. The duration is the max time to wait for a
// response.
func PingTTRPC(rw net.Conn) error {
	n, err := rw.Write([]byte{
		0, 0, 0, 0, // Zero length
		0, 0, 0, 0, // Zero stream ID to force rejection response (must be odd)
		0, 0, // No type or flags
	})
	if err != nil {
		return fmt.Errorf("failed to write to TTRPC server: %w", err)
	} else if n != 10 {
		return fmt.Errorf("short write: %d bytes written", n)
	}
	p := make([]byte, 10)
	_, err = io.ReadFull(rw, p)
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(p[:4])
	sid := binary.BigEndian.Uint32(p[4:8])
	if sid != 0 {
		return fmt.Errorf("unexpected stream ID %d, expected 0", sid)
	}

	if length == 0 {
		return fmt.Errorf("expected error response, but got length 0")
	}

	_, err = io.Copy(io.Discard, io.LimitReader(rw, int64(length)))
	return err
}
