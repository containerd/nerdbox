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
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
)

// NetworkSuite contains conformance tests for a container's default
// network connectivity, gated on the "net" feature.
//
// These tests are deliberately agnostic to how a shim provides outbound
// connectivity (a host network namespace, a virtual NIC, a userspace proxy,
// or any other mechanism). They verify only the externally observable
// contract: a container started without any network configuration must
// behave like a process with working outbound networking — able to open
// TCP connections, exchange UDP datagrams, and resolve DNS names — exactly
// as "host networking" (no network namespace / no isolation) would provide.
// The mechanism a shim uses internally to satisfy this contract is not
// part of the API surface these tests check.
type NetworkSuite struct {
	cfg Config
}

// NewNetworkSuite constructs a NetworkSuite from the given options.
func NewNetworkSuite(cfg Config) *NetworkSuite {
	return &NetworkSuite{cfg: cfg}
}

// Run runs every test in the suite as a subtest of t.
func (s *NetworkSuite) Run(t *testing.T) {
	t.Helper()
	registerShimLeakCheck(t, s.cfg.ShimBinary)
	t.Run("OutboundTCP", s.testOutboundTCP)
	t.Run("OutboundUDP", s.testOutboundUDP)
	t.Run("DNSResolve", s.testDNSResolve)
}

// testOutboundTCP verifies that a container's init process can open an
// outbound TCP connection to a host-reachable endpoint and complete a
// round trip.
//
// The API contract: a container started without any network configuration
// must have a working default network path for outbound TCP, the same way
// a process using the host's network stack would. This test does not care
// how connectivity is achieved.
//
// The container runs nc(1) in TCP mode (nc <host> <port>), which copies
// stdin to the connection and the connection to stdout. The test writes a
// token to the container's stdin FIFO; the host echoes it back; the test
// asserts that the container's stdout contains the echoed token.
func (s *NetworkSuite) testOutboundTCP(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("host tcp listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	const token = "network-suite-tcp-ok"
	acceptDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptDone <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 256)
		n, err := conn.Read(buf)
		if err != nil && n == 0 {
			acceptDone <- err
			return
		}
		got := strings.TrimSpace(string(buf[:n]))
		if got != token {
			acceptDone <- fmt.Errorf("host received %q, want %q", got, token)
			return
		}
		if _, err := fmt.Fprintf(conn, "%s\n", token); err != nil {
			acceptDone <- err
			return
		}
		// Close so nc's io.Copy from the socket returns and the container exits.
		conn.Close()
		acceptDone <- nil
	}()

	// nc <host> <port>: TCP mode, bidirectional pipe between socket and stdio.
	createOCISpec(t, bundleDir, []string{"/bin/nc", host, port}, s.cfg)

	stdinPath, stdoutPath, stderrPath := createStdioFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequestStdin(t, cid, bundleDir, stdinPath, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	// Write the token to the container's stdin so nc sends it to the host.
	stdinFifo, err := openPipeWriter(ctx, stdinPath)
	if err != nil {
		t.Fatalf("open stdin fifo: %v", err)
	}
	if _, err := fmt.Fprintf(stdinFifo, "%s\n", token); err != nil {
		t.Fatalf("write token to stdin: %v", err)
	}
	stdinFifo.Close()

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatal("wait failed:", err)
	}
	<-stdoutDone
	stdoutMu.Lock()
	out := stdoutBuf.String()
	stdoutMu.Unlock()

	if waitResp.ExitStatus != 0 {
		t.Fatalf("container exit status: got %d, want 0; stdout: %q", waitResp.ExitStatus, out)
	}
	if !strings.Contains(out, token) {
		t.Errorf("container stdout: want %q in output, got %q", token, out)
	}

	select {
	case err := <-acceptDone:
		if err != nil {
			t.Fatalf("host accept/echo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not observe a connection from the container within 5s")
	}

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	shutdownTask(ctx, tc, cid)
	t.Log("container outbound TCP round-trip: ok")
}

// testOutboundUDP verifies that a container's init process can exchange a
// UDP datagram with a host-reachable endpoint.
//
// The API contract: a container started without any network configuration
// must support outbound UDP datagrams, the same way a process using the
// host's network stack would.
//
// The container runs nc(1) in UDP mode (nc -u <host> <port>), which sends
// stdin as a single datagram and writes the reply datagram to stdout. The
// test writes a token to the container's stdin FIFO; the host echoes it back;
// the test asserts that the container's stdout contains the echoed token.
func (s *NetworkSuite) testOutboundUDP(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("host udp listen: %v", err)
	}
	t.Cleanup(func() { pc.Close() })

	host, port, err := net.SplitHostPort(pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	const token = "network-suite-udp-ok"
	recvDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		pc.SetDeadline(time.Now().Add(10 * time.Second))
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			recvDone <- err
			return
		}
		got := strings.TrimSpace(string(buf[:n]))
		if got != token {
			recvDone <- fmt.Errorf("host received %q, want %q", got, token)
			return
		}
		if _, err := pc.WriteTo([]byte(token), addr); err != nil {
			recvDone <- err
			return
		}
		recvDone <- nil
	}()

	// nc -u <host> <port>: UDP mode, one sendto (stdin) then one recvfrom (stdout).
	createOCISpec(t, bundleDir, []string{"/bin/nc", "-u", host, port}, s.cfg)

	stdinPath, stdoutPath, stderrPath := createStdioFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequestStdin(t, cid, bundleDir, stdinPath, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	// Write the token to stdin and close so nc reads EOF and sends one datagram.
	stdinFifo, err := openPipeWriter(ctx, stdinPath)
	if err != nil {
		t.Fatalf("open stdin fifo: %v", err)
	}
	if _, err := fmt.Fprint(stdinFifo, token); err != nil {
		t.Fatalf("write token to stdin: %v", err)
	}
	stdinFifo.Close()

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatal("wait failed:", err)
	}
	<-stdoutDone
	stdoutMu.Lock()
	out := stdoutBuf.String()
	stdoutMu.Unlock()

	if waitResp.ExitStatus != 0 {
		t.Fatalf("container exit status: got %d, want 0; stdout: %q", waitResp.ExitStatus, out)
	}
	if !strings.Contains(out, token) {
		t.Errorf("container stdout: want %q in output, got %q", token, out)
	}

	select {
	case err := <-recvDone:
		if err != nil {
			t.Fatalf("host receive/echo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not observe a datagram from the container within 5s")
	}

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	shutdownTask(ctx, tc, cid)
	t.Log("container outbound UDP round-trip: ok")
}

// dnsTestHostname is the hostname resolved by testDNSResolve. Resolving
// "localhost" would not exercise the network path at all — it is normally
// answered from /etc/hosts or the resolver's static handling without any
// DNS query. example.com is IANA-reserved for documentation and testing,
// extremely stable, and forces a real DNS query over the container's
// default network path. This is the one test in this suite that requires
// outbound internet access from the test host.
const dnsTestHostname = "example.com"

// testDNSResolve verifies that a container's init process can resolve a
// hostname using the same DNS configuration the host would use.
//
// The API contract: a container started without any network configuration
// must be able to resolve DNS names via a resolver reachable through its
// default network path (typically the host's own resolver configuration,
// copied into the container when no explicit network is configured).
//
// The container runs host(1) (host <name>), which prints one line per address
// in the form "<name> has address <ip>". The test parses those lines and
// validates that each address field is a valid IP address.
func (s *NetworkSuite) testDNSResolve(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	// host <name>: prints "<name> has address <ip>" for each resolved address.
	createOCISpec(t, bundleDir, []string{"/bin/host", dnsTestHostname}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	var stderrBuf bytes.Buffer
	var stderrMu sync.Mutex
	drainFifoInto(t, ctx, stderrPath, &stderrBuf, &stderrMu)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatal("wait failed:", err)
	}
	<-stdoutDone
	stdoutMu.Lock()
	out := stdoutBuf.String()
	stdoutMu.Unlock()
	stderrMu.Lock()
	errOut := stderrBuf.String()
	stderrMu.Unlock()

	if waitResp.ExitStatus != 0 {
		t.Fatalf("container exit status: got %d, want 0; stdout: %q, stderr: %q", waitResp.ExitStatus, out, errOut)
	}

	// Parse "example.com has address 93.184.216.34" lines and validate each IP.
	var addrs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Expected format: "<name> has address <ip>"
		const marker = " has address "
		idx := strings.Index(line, marker)
		if idx < 0 {
			t.Errorf("host %s: unexpected output line %q", dnsTestHostname, line)
			continue
		}
		ip := line[idx+len(marker):]
		if net.ParseIP(ip) == nil {
			t.Errorf("host %s: %q is not a valid IP address", dnsTestHostname, ip)
			continue
		}
		addrs = append(addrs, ip)
	}
	if len(addrs) == 0 {
		t.Fatalf("host %s produced no addresses; stderr: %q", dnsTestHostname, errOut)
	}

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	shutdownTask(ctx, tc, cid)
	t.Log("container DNS resolution: ok, addresses:", addrs)
}
