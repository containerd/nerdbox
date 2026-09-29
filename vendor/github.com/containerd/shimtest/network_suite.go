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
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// networkSuitePortRangeStart and networkSuitePortRangeEnd bound the
// candidates pickHostPort chooses testInboundTCPListen's port from.
// The range is deliberately outside Linux's default ephemeral port
// range (typically 32768-60999), to reduce the chance of a candidate
// colliding with an unrelated connection's OS-assigned source port.
const (
	networkSuitePortRangeStart = 20000
	networkSuitePortRangeEnd   = 29999
	networkSuitePortAttempts   = 20
)

// pickHostPort returns a TCP port on 127.0.0.1 verified free at the
// moment of the call, by binding and immediately releasing a listener.
// testInboundTCPListen uses this instead of requesting port 0 (letting
// the container's own kernel assign an ephemeral port), for two
// reasons:
//
//   - With Config.ProvideNetwork, the host-side inbound forward must be
//     registered before Task.Start -- before the container's listener
//     even exists -- so the port cannot be discovered from the
//     container at runtime; it has to be chosen ahead of time.
//   - More fundamentally, some shims proxy each socket call to the host
//     independently rather than truly sharing the host's network stack
//     (see containerHostAddr's doc and testLoopbackWithinContainer).
//     For such a shim, binding port 0 lets the guest and the host each
//     resolve "an ephemeral port" on their own, and the two are not
//     guaranteed to agree: the application only ever observes the
//     guest's number, which need not be the port the host is actually
//     listening on. Requesting the same concrete port on both sides
//     removes that ambiguity entirely, regardless of the shim's
//     networking mechanism.
//
// Binding a random port from a fixed range, with a retry on conflict,
// rather than a single hardcoded constant, avoids the test failing
// outright should that one port already be in use -- e.g. a concurrent
// run of this suite on the same host. This is inherently racy (another
// process could take the port between release and the container's own
// bind), but is a reasonable, standard mitigation for a test suite;
// this suite's tests run sequentially (never in parallel with each
// other), so at least a prior iteration of this same suite cannot be
// the collision.
func pickHostPort(t *testing.T) int {
	t.Helper()
	for range networkSuitePortAttempts {
		port := networkSuitePortRangeStart + rand.IntN(networkSuitePortRangeEnd-networkSuitePortRangeStart+1)
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		ln.Close()
		return port
	}
	t.Fatalf("pickHostPort: no free port in %d-%d after %d attempts",
		networkSuitePortRangeStart, networkSuitePortRangeEnd, networkSuitePortAttempts)
	return 0
}

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
	t.Run("InboundTCPListen", s.testInboundTCPListen)
	t.Run("LoopbackWithinContainer", s.testLoopbackWithinContainer)
}

// containerHostAddr returns the address a member of this suite's
// containers should use to reach a host-bound listener: slirpHostAddr
// when the test itself is providing the container's networking (see
// attachContainerNetwork), or the host's own loopback when the shim is
// assumed to already give the container a working default network path
// (e.g. by sharing the host's network namespace).
func (s *NetworkSuite) containerHostAddr() string {
	if s.cfg.ProvideNetwork {
		return slirpHostAddr
	}
	return "127.0.0.1"
}

// networkSpecOpts returns the CreateOCISpec opts needed for a
// container to get its own network namespace, when this suite is
// responsible for providing that container's networking itself
// (Config.ProvideNetwork). Returns nil otherwise: the shim is assumed
// to already provide a default network path with no special spec
// configuration required.
func (s *NetworkSuite) networkSpecOpts() []func(*specs.Spec) {
	if !s.cfg.ProvideNetwork {
		return nil
	}
	return []func(*specs.Spec){withNewNetworkNamespace()}
}

// attachIfProvided attaches this suite's own container networking (see
// attachContainerNetwork) after Task.Create when Config.ProvideNetwork
// is set, and is a no-op otherwise.
func (s *NetworkSuite) attachIfProvided(t *testing.T, pid uint32) *containerNetwork {
	t.Helper()
	if !s.cfg.ProvideNetwork {
		return nil
	}
	return attachContainerNetwork(t, pid)
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
//
// When Config.ProvideNetwork is set, connectivity is provided by the test
// itself (see attachContainerNetwork): the container reaches the host at
// slirpHostAddr, which slirp4netns proxies to the host's own 127.0.0.1,
// exactly where the listener below is bound. Otherwise the shim is assumed
// to already give the container a default network path reaching the host
// directly at 127.0.0.1.
func (s *NetworkSuite) testOutboundTCP(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("host tcp listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	_, port, err := net.SplitHostPort(ln.Addr().String())
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
	createOCISpec(t, bundleDir, []string{"/bin/nc", s.containerHostAddr(), port}, s.cfg, s.networkSpecOpts()...)

	stdinPath, stdoutPath, stderrPath := createStdioFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := newTaskClient(client, params.Version)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	// Open the stdin pipe writer before Create. On Windows the shim dials
	// the stdin named pipe synchronously during the Create RPC, so the
	// server-side listener must already be accepting before that call is
	// issued — exactly the same ordering constraint as with exec stdin in
	// testStdinDetachReattach. openPipeWriter starts the Accept in a
	// background goroutine so the call returns immediately.
	stdinFifo, err := openPipeWriter(ctx, stdinPath)
	if err != nil {
		t.Fatalf("open stdin fifo: %v", err)
	}

	createResp, err := tc.Create(ctx, newCreateTaskRequestStdin(t, cid, bundleDir, stdinPath, stdoutPath, stderrPath, rootfsMounts))
	if err != nil {
		t.Fatal("create failed:", err)
	}
	// Attach networking (when we're responsible for providing it) after
	// Create (so the container's network namespace already exists) and
	// before Start (so nc's very first connect() sees a fully configured
	// network) — see attachContainerNetwork.
	s.attachIfProvided(t, createResp.GetPid())
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	// Write the token to the container's stdin so nc sends it to the host.
	if _, err := fmt.Fprintf(stdinFifo, "%s\n", token); err != nil {
		t.Fatalf("write token to stdin: %v", err)
	}
	stdinFifo.Close()

	// Signal EOF to nc via the CloseIO RPC. Closing the test's own FIFO
	// write end alone is not sufficient: the shim holds its own write-end
	// reference on the stdin FIFO and only releases it upon CloseIO,
	// exactly as documented for exec stdin in exec_suite.go. Without this
	// call the container's init process would never observe stdin EOF.
	if _, err := tc.CloseIO(ctx, &taskAPI.CloseIORequest{ID: cid, Stdin: true}); err != nil {
		t.Fatal("close stdin failed:", err)
	}

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

	_, port, err := net.SplitHostPort(pc.LocalAddr().String())
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

	// nc -u <host> <port>: UDP mode, one sendto (stdin) then one recvfrom
	// (stdout). See testOutboundTCP for containerHostAddr/networkSpecOpts.
	createOCISpec(t, bundleDir, []string{"/bin/nc", "-u", s.containerHostAddr(), port}, s.cfg, s.networkSpecOpts()...)

	stdinPath, stdoutPath, stderrPath := createStdioFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := newTaskClient(client, params.Version)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	drainFifo(t, ctx, stderrPath)

	// Open the stdin pipe writer before Create for the same reason as
	// testOutboundTCP: the shim dials stdin synchronously during Create on
	// Windows, so the listener must be accepting before that RPC is issued.
	stdinFifo, err := openPipeWriter(ctx, stdinPath)
	if err != nil {
		t.Fatalf("open stdin fifo: %v", err)
	}

	createResp, err := tc.Create(ctx, newCreateTaskRequestStdin(t, cid, bundleDir, stdinPath, stdoutPath, stderrPath, rootfsMounts))
	if err != nil {
		t.Fatal("create failed:", err)
	}
	s.attachIfProvided(t, createResp.GetPid())
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	// Write the token to stdin and close so nc reads EOF and sends one datagram.
	if _, err := fmt.Fprint(stdinFifo, token); err != nil {
		t.Fatalf("write token to stdin: %v", err)
	}
	stdinFifo.Close()

	// Signal EOF to nc via the CloseIO RPC; see testOutboundTCP for why
	// closing the test's own FIFO write end alone is not sufficient.
	if _, err := tc.CloseIO(ctx, &taskAPI.CloseIORequest{ID: cid, Stdin: true}); err != nil {
		t.Fatal("close stdin failed:", err)
	}

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
//
// When Config.ProvideNetwork is set, connectivity is provided by the test
// itself (see attachContainerNetwork), whose resolver is slirpDNSAddr; the
// container's /etc/resolv.conf is bind-mounted to point at it, since the
// embedded rootfs carries none of its own. Otherwise the shim is assumed to
// already give the container a default network path with working DNS.
func (s *NetworkSuite) testDNSResolve(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	specOpts := s.networkSpecOpts()
	if s.cfg.ProvideNetwork {
		resolvConfPath := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(resolvConfPath, []byte("nameserver "+slirpDNSAddr+"\n"), 0o644); err != nil {
			t.Fatalf("write resolv.conf: %v", err)
		}
		specOpts = append(specOpts, withExtraMounts(specs.Mount{
			Type:        "bind",
			Source:      resolvConfPath,
			Destination: "/etc/resolv.conf",
			// Not "ro": a read-only bind mount requires a follow-up
			// MS_REMOUNT|MS_RDONLY, which fails EPERM in a rootless
			// user namespace. Read-write is an acceptable tradeoff here
			// -- this file is test-owned scratch data, not a security
			// boundary the container has any reason to tamper with.
			Options: []string{"rbind"},
		}))
	}

	// host <name>: prints "<name> has address <ip>" for each resolved address.
	createOCISpec(t, bundleDir, []string{"/bin/host", dnsTestHostname}, s.cfg, specOpts...)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := newTaskClient(client, params.Version)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	var stderrBuf bytes.Buffer
	var stderrMu sync.Mutex
	drainFifoInto(t, ctx, stderrPath, &stderrBuf, &stderrMu)

	createResp, err := tc.Create(ctx, newCreateTaskRequest(t, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts))
	if err != nil {
		t.Fatal("create failed:", err)
	}
	s.attachIfProvided(t, createResp.GetPid())
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

// testInboundTCPListen verifies that a container's init process can bind a
// TCP listener and accept a connection from outside the container — i.e. that
// the shim's default networking path exposes inbound TCP as well as outbound.
//
// The API contract: a container started without any network configuration must
// be able to bind a TCP listener and receive connections from the host, the
// same way a process using the host's network stack would. The shim
// documentation claims that "ports bound inside the VM are transparently
// mapped on the host"; this test verifies that claim is true in practice.
//
// The container binds a concrete port chosen by pickHostPort — never port
// 0 — regardless of Config.ProvideNetwork. See pickHostPort's doc for why
// a discovered ephemeral port is not just unnecessary but actively unsafe
// to rely on for some shims: the guest and the host can each resolve "an
// ephemeral port" independently and disagree, so the application's own
// report of its bound port is not guaranteed to be where the host is
// actually listening. With Config.ProvideNetwork the fixed port is also
// what lets the test register its own inbound port forward (see
// attachContainerNetwork.AddInboundForward) before Task.Start, before the
// container's listener even exists.
//
// nc -v reports the socket it bound on stderr before accepting; the test
// waits for that line purely as a readiness signal (the port itself is
// already known) before dialing host-loopback (127.0.0.1:<port>) and
// sending a token. nc copies its connection to stdout and its stdin to the
// connection, but has no path that echoes received data back over the same
// connection — so the test confirms the token actually reached the
// container, which is this test's whole point, by waiting for it to appear
// in the container's own captured stdout rather than by reading a reply off
// the socket. The test hard-fails if the host cannot connect: inbound
// reachability is a required contract of the default networking path.
func (s *NetworkSuite) testInboundTCPListen(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	port := pickHostPort(t)
	requestedPort := strconv.Itoa(port)

	// nc -v -l <port>: bind, report the bound socket on stderr, accept one
	// connection, then bidirectionally pipe stdio↔socket. stdout is left
	// carrying connection payload only, so the token assertion below cannot
	// be confused by control output.
	createOCISpec(t, bundleDir, []string{"/bin/nc", "-v", "-l", requestedPort}, s.cfg, s.networkSpecOpts()...)

	stdinPath, stdoutPath, stderrPath := createStdioFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := newTaskClient(client, params.Version)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	var stderrBuf bytes.Buffer
	var stderrMu sync.Mutex
	drainFifoInto(t, ctx, stderrPath, &stderrBuf, &stderrMu)

	createResp, err := tc.Create(ctx, newCreateTaskRequestStdin(t, cid, bundleDir, stdinPath, stdoutPath, stderrPath, rootfsMounts))
	if err != nil {
		t.Fatal("create failed:", err)
	}
	netw := s.attachIfProvided(t, createResp.GetPid())
	if s.cfg.ProvideNetwork {
		// Register the forward before Start: the port is fixed and known
		// ahead of time, so there is no need to wait for the container to
		// report it first, and no window in which an early host
		// connection attempt could race the forward's registration.
		netw.AddInboundForward(t, port, port)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	// Read the bound port from the container's stderr. nc -v reports the
	// listening socket there before calling Accept, so this completes as
	// soon as the listener is ready. A 15 s timeout guards against hangs.
	// The port is already known (see pickHostPort); this is a readiness
	// signal, and doubles as a check that the container actually bound
	// the exact port it was asked to.
	boundPort := waitForListeningPort(t, &stderrBuf, &stderrMu, 15*time.Second)
	if boundPort != requestedPort {
		t.Fatalf("container reported port %q, want %q", boundPort, requestedPort)
	}
	t.Logf("container listener bound on port %s", boundPort)

	// Dial the container's mapped/forwarded port on the host loopback.
	hostAddr := net.JoinHostPort("127.0.0.1", boundPort)
	const token = "network-suite-inbound-ok"

	hostConn, err := net.DialTimeout("tcp", hostAddr, 15*time.Second)
	if err != nil {
		t.Fatalf("host could not connect to container listener at %s: %v\n"+
			"(inbound port mapping is a required contract of the default networking path)", hostAddr, err)
	}
	hostConn.SetDeadline(time.Now().Add(10 * time.Second))

	// Send the token to the container.
	if _, err := fmt.Fprintf(hostConn, "%s\n", token); err != nil {
		t.Fatalf("host write to container: %v", err)
	}

	// Confirm the token actually reached the container by waiting for it
	// to show up in the container's own stdout, rather than reading a
	// reply back over the socket (nc -l has no echo direction — see
	// above).
	tokenDeadline := time.After(10 * time.Second)
	var out string
	for {
		stdoutMu.Lock()
		out = stdoutBuf.String()
		stdoutMu.Unlock()
		if strings.Contains(out, token) {
			break
		}
		select {
		case <-tokenDeadline:
			t.Fatalf("container did not report receiving %q within 10s; stdout so far: %q", token, out)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Close the host connection so nc's io.Copy from the connection returns.
	hostConn.Close()

	// Write nothing to stdin — just close it so nc's stdin→socket copy also
	// finishes; combined with hostConn.Close() nc will exit cleanly.
	stdinFifo, err := openPipeWriter(ctx, stdinPath)
	if err != nil {
		t.Fatalf("open stdin fifo: %v", err)
	}
	stdinFifo.Close()

	// Signal EOF to nc via the CloseIO RPC; see testOutboundTCP for why
	// closing the test's own FIFO write end alone is not sufficient.
	if _, err := tc.CloseIO(ctx, &taskAPI.CloseIORequest{ID: cid, Stdin: true}); err != nil {
		t.Fatal("close stdin failed:", err)
	}

	waitResp, err := tc.Wait(ctx, &taskAPI.WaitRequest{ID: cid})
	if err != nil {
		t.Fatal("wait failed:", err)
	}
	<-stdoutDone
	stdoutMu.Lock()
	out = stdoutBuf.String()
	stdoutMu.Unlock()

	if waitResp.ExitStatus != 0 {
		t.Fatalf("container exit status: got %d, want 0; stdout: %q", waitResp.ExitStatus, out)
	}

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	shutdownTask(ctx, tc, cid)
	t.Logf("container inbound TCP listen: ok (port %s)", boundPort)
}

// testLoopbackWithinContainer verifies that a container's init process can
// connect to itself over loopback (127.0.0.1). This guards the in-container
// localhost contract: a process that binds 127.0.0.1 inside a container must
// be reachable from another process within the same container via loopback.
//
// The API contract: a container started without any network configuration must
// have a working loopback interface, exactly as any process running on the
// host's network stack would. This is a prerequisite for between-container
// localhost connectivity: if the loopback inside a single container is broken,
// containers sharing a network namespace will also fail.
//
// The container runs a small helper binary ("looptest"; see cmdLooptest in
// testbin) that binds a listener, then connects back to it over
// 127.0.0.1:<port> from within the same process. Because both ends run in
// the same init process, only in-container loopback carries the traffic.
// looptest binds a concrete port from a fixed range with a short retry
// loop, rather than port 0, for the same reason pickHostPort exists on the
// host side: some shims proxy each socket call to the host independently,
// so a port 0 bind's two ends could each resolve "an ephemeral port" to a
// different number and never actually rendezvous.
//
// This test doesn't need attachContainerNetwork's connectivity itself (it
// never leaves the container), but when Config.ProvideNetwork is set it
// still requests a network namespace and attaches to it like every other
// test in this suite, to confirm that in-container loopback keeps working
// under exactly the same namespace setup the tests that do need outside
// connectivity depend on.
func (s *NetworkSuite) testLoopbackWithinContainer(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)
	cid := containerID(t)

	const token = "network-suite-loopback-ok"

	// looptest <token>: binds a listener on a concrete port (see
	// looptestPortRangeStart in testbin), then connects back to it from
	// within the same process, sends the token, and prints the echoed
	// response to stdout.
	createOCISpec(t, bundleDir, []string{"/bin/looptest", token}, s.cfg, s.networkSpecOpts()...)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "net")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)
	shimConn := connectShim(t, params.Address)
	client := ttrpc.NewClient(shimConn)
	defer client.Close()

	tc := newTaskClient(client, params.Version)

	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex
	stdoutDone := drainFifoIntoDone(t, ctx, stdoutPath, &stdoutBuf, &stdoutMu)
	var stderrBuf bytes.Buffer
	var stderrMu sync.Mutex
	drainFifoInto(t, ctx, stderrPath, &stderrBuf, &stderrMu)

	createResp, err := tc.Create(ctx, newCreateTaskRequest(t, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts))
	if err != nil {
		t.Fatal("create failed:", err)
	}
	s.attachIfProvided(t, createResp.GetPid())
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
	if !strings.Contains(out, token) {
		t.Fatalf("loopback echo not found in stdout: want %q, got %q; stderr: %q", token, out, errOut)
	}

	tc.Delete(ctx, &taskAPI.DeleteRequest{ID: cid})
	shutdownTask(ctx, tc, cid)
	t.Log("container in-container loopback: ok")
}

// listeningNotice is the prefix nc(1) uses to report the socket it bound
// when run with -v. The full line has the form "Listening on <addr> <port>".
const listeningNotice = "Listening on "

// waitForListeningPort polls buf (protected by mu) until nc's -v listen
// notice appears in the accumulated output, then returns the port it
// reports. It lets a test synchronously learn the port a container bound
// without opening a competing reader on the FIFO — the caller's drainFifo
// goroutine already owns the read end.
//
// Only newline-terminated lines are considered, so a port number still being
// written cannot be parsed as if it were complete.
func waitForListeningPort(t *testing.T, buf *bytes.Buffer, mu *sync.Mutex, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		mu.Lock()
		out := buf.String()
		mu.Unlock()
		if end := strings.LastIndex(out, "\n"); end >= 0 {
			for _, line := range strings.Split(out[:end], "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, listeningNotice) {
					continue
				}
				if fields := strings.Fields(line); len(fields) >= 2 {
					return fields[len(fields)-1]
				}
			}
		}
		select {
		case <-deadline:
			mu.Lock()
			final := buf.String()
			mu.Unlock()
			t.Fatalf("timed out after %v waiting for nc's %q notice; stderr so far: %q",
				timeout, strings.TrimSpace(listeningNotice), final)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
