//go:build linux

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

package shimtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The fixed addresses slirp4netns assigns when attached with --configure:
// slirpGuestAddr is the container-side tap address, slirpHostAddr is the
// built-in gateway that proxies to the host's own loopback (127.0.0.1),
// and slirpDNSAddr is slirp's internal DNS proxy.
const (
	slirpGuestAddr = "10.0.2.100"
	slirpHostAddr  = "10.0.2.2"
	slirpDNSAddr   = "10.0.2.3"
)

// containerNetwork represents connectivity attached to a single
// container's network namespace via a dedicated slirp4netns process.
type containerNetwork struct {
	cmd       *exec.Cmd
	apiSocket string
}

// attachContainerNetwork attaches slirp4netns to the network namespace
// of the container process identified by initPID (as returned by
// Task.Create's CreateTaskResponse.Pid), giving the container outbound
// connectivity to the host (reachable at slirpHostAddr) and enabling
// inbound port forwards via AddInboundForward. Works identically for
// root and rootless shims: slirp4netns needs no special privileges of
// its own, only a target network namespace to attach to (plus
// --userns-path when that namespace's owning user namespace isn't the
// caller's own, i.e. whenever the shim under test isn't running as
// root).
//
// Must be called after Task.Create (so initPID's network namespace
// already exists) and before Task.Start (so the container's entrypoint
// sees a fully configured network from its very first syscall). This
// ordering, together with slirp4netns's --ready-fd, makes the setup
// race-free: the test blocks on a pipe that slirp4netns writes to only
// once the interface is fully configured, so no retry logic is needed
// in the container's own networking code.
//
// Skips the test if slirp4netns is not on PATH: providing container
// networking is the test harness's job here, not the shim's, so a
// missing tool is an environment limitation, not a conformance
// failure.
func attachContainerNetwork(tb testing.TB, initPID uint32) *containerNetwork {
	tb.Helper()

	slirpPath, err := exec.LookPath("slirp4netns")
	if err != nil {
		tb.Skip("net tests require slirp4netns (not found on PATH) to provide container networking")
	}

	apiSocket := filepath.Join(tb.TempDir(), "slirp-api.sock")

	readyR, readyW, err := os.Pipe()
	if err != nil {
		tb.Fatalf("attachContainerNetwork: pipe: %v", err)
	}
	defer readyW.Close()

	args := []string{
		"--configure",
		"--api-socket", apiSocket,
		"--ready-fd", "3",
	}
	if os.Getuid() != 0 {
		// The container's network namespace is owned by its own user
		// namespace when the shim isn't running as root; slirp4netns
		// must enter that user namespace to configure the interface.
		args = append(args, "--userns-path", fmt.Sprintf("/proc/%d/ns/user", initPID))
	}
	args = append(args, strconv.FormatUint(uint64(initPID), 10), "tap0")

	cmd := exec.Command(slirpPath, args...)
	cmd.ExtraFiles = []*os.File{readyW}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		tb.Fatalf("attachContainerNetwork: start slirp4netns: %v", err)
	}
	// This explicit close is what actually matters: the parent must drop
	// its own copy of the write end now that the child has inherited one
	// via ExtraFiles, or the parent's lingering reference keeps the pipe
	// open and readyR's Read below blocks forever if slirp4netns exits
	// without ever writing to it. The deferred Close above still runs
	// after this, but by then the fd is already closed — Close is safe
	// to call twice in Go, so that's a harmless no-op, not a double
	// free. Its only real job is covering the path that returns before
	// reaching here: cmd.Start's own failure just above.
	readyW.Close()

	readyErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := readyR.Read(buf)
		readyErr <- err
	}()

	select {
	case err := <-readyErr:
		if err != nil {
			cmd.Process.Kill() //nolint:errcheck
			tb.Fatalf("attachContainerNetwork: waiting for slirp4netns readiness: %v; stderr: %s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		tb.Fatalf("attachContainerNetwork: slirp4netns did not become ready within 10s; stderr: %s", stderr.String())
	}
	readyR.Close()

	tb.Cleanup(func() {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})

	return &containerNetwork{cmd: cmd, apiSocket: apiSocket}
}

// AddInboundForward registers a slirp4netns port forward so that a
// connection to the host at 127.0.0.1:hostPort is delivered to the
// container at guestPort. Because it targets a fixed, predetermined
// port rather than one discovered at runtime, this can (and should) be
// called before Task.Start: the forward is in place before the
// container's listener even exists, so there is no window in which an
// early host-side connection attempt could race the forward's
// registration.
func (n *containerNetwork) AddInboundForward(tb testing.TB, hostPort, guestPort int) {
	tb.Helper()

	req := map[string]any{
		"execute": "add_hostfwd",
		"arguments": map[string]any{
			"proto":      "tcp",
			"host_addr":  "127.0.0.1",
			"host_port":  hostPort,
			"guest_addr": slirpGuestAddr,
			"guest_port": guestPort,
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		tb.Fatalf("AddInboundForward: marshal request: %v", err)
	}

	conn, err := net.Dial("unix", n.apiSocket)
	if err != nil {
		tb.Fatalf("AddInboundForward: dial slirp4netns API socket: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(body); err != nil {
		tb.Fatalf("AddInboundForward: write request: %v", err)
	}
	// The API protocol has no keep-alive or request framing beyond the
	// JSON body itself; the client signals the end of the request by
	// shutting down the write side of the connection.
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			tb.Fatalf("AddInboundForward: close write side: %v", err)
		}
	}

	resp, err := io.ReadAll(conn)
	if err != nil {
		tb.Fatalf("AddInboundForward: read response: %v", err)
	}

	var result struct {
		Return map[string]any `json:"return"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		tb.Fatalf("AddInboundForward: unmarshal response %q: %v", resp, err)
	}
	if result.Error != nil {
		tb.Fatalf("AddInboundForward: slirp4netns error: %v", result.Error)
	}
}
