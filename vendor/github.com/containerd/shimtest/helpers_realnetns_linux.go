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
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// realNetworkSandbox is a genuine Linux network namespace created via the
// system "ip netns" tool — a standard unshare(CLONE_NEWNET) + bind-mount
// technique for creating an isolated, enterable network namespace.
// Unlike the fake-file sandbox used by createNetworkSandbox, a
// realNetworkSandbox can be entered with setns(2) and can carry real network
// interfaces, so it can be used to observe where a shim's network traffic
// actually originates.
//
// Creating one requires CAP_SYS_ADMIN (root) in the initial user namespace;
// callers should be prepared for createRealNetworkSandbox to skip the test.
type realNetworkSandbox struct {
	name string
	path string
}

var (
	netnsCounter      atomic.Int32
	vethSubnetCounter atomic.Int32
)

// createRealNetworkSandbox creates a genuine network namespace using the
// system "ip netns add" command. Cleanup (namespace deletion, which also
// destroys any interfaces that live only inside it) is registered on tb.
//
// Skips the test if not running as root: creating and entering a real
// network namespace requires CAP_SYS_ADMIN.
func createRealNetworkSandbox(tb testing.TB) *realNetworkSandbox {
	tb.Helper()
	if os.Getuid() != 0 {
		tb.Skip("createRealNetworkSandbox requires root (CAP_SYS_ADMIN)")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		tb.Skip("createRealNetworkSandbox requires the \"ip\" (iproute2) command")
	}

	name := fmt.Sprintf("shimtest-%d-%d", os.Getpid(), netnsCounter.Add(1))
	if out, err := exec.Command("ip", "netns", "add", name).CombinedOutput(); err != nil {
		tb.Fatalf("ip netns add %s: %v: %s", name, err, out)
	}
	tb.Cleanup(func() {
		exec.Command("ip", "netns", "delete", name).Run() //nolint:errcheck
	})

	return &realNetworkSandbox{name: name, path: "/var/run/netns/" + name}
}

// attachVeth creates a veth pair with both ends inside the sandbox namespace
// and assigns one end a unique address on a small point-to-point subnet.
// Because neither end of the pair ever exists in the root namespace, the
// resulting subnet has no route from outside the sandbox: only a process
// actually running inside the sandbox namespace (or a caller that has
// entered it via setns) can reach the returned address. This is what makes
// the address usable as a falsifiable probe for "did this traffic originate
// inside the network sandbox".
//
// The interfaces are destroyed automatically when the sandbox namespace is
// deleted; no separate cleanup is required.
func (n *realNetworkSandbox) attachVeth(tb testing.TB) (addr string) {
	tb.Helper()

	slot := vethSubnetCounter.Add(1) % 250
	addr = fmt.Sprintf("10.244.%d.1", slot)
	hostIf := fmt.Sprintf("vh%d", slot)
	peerIf := fmt.Sprintf("vp%d", slot)

	runIP(tb, "link", "add", hostIf, "type", "veth", "peer", "name", peerIf)
	runIP(tb, "link", "set", hostIf, "netns", n.name)
	runIP(tb, "link", "set", peerIf, "netns", n.name)

	runIPNetns(tb, n.name, "addr", "add", addr+"/30", "dev", hostIf)
	runIPNetns(tb, n.name, "link", "set", hostIf, "up")
	runIPNetns(tb, n.name, "link", "set", peerIf, "up")
	runIPNetns(tb, n.name, "link", "set", "lo", "up")

	return addr
}

// runIP runs "ip <args...>" and fails the test on error.
func runIP(tb testing.TB, args ...string) {
	tb.Helper()
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		tb.Fatalf("ip %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// runIPNetns runs "ip netns exec <name> ip <args...>" and fails the test on
// error.
func runIPNetns(tb testing.TB, name string, args ...string) {
	tb.Helper()
	full := append([]string{"netns", "exec", name, "ip"}, args...)
	if out, err := exec.Command("ip", full...).CombinedOutput(); err != nil {
		tb.Fatalf("ip netns exec %s ip %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
}

// probeUnreachableFromCurrentNamespace asserts that addr cannot be reached
// from the calling goroutine's current network namespace. It is used as a
// sanity check that a realNetworkSandbox address is actually isolated
// before relying on it as a falsifiable probe.
func probeUnreachableFromCurrentNamespace(tb testing.TB, addr string) {
	tb.Helper()
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		conn.Close()
		tb.Fatalf("test setup error: %s is reachable from the current namespace; "+
			"the sandbox isolation this test relies on is not actually in effect", addr)
	}
}

// listenAndEchoOnceInNetns enters the network namespace at nsPath on a
// dedicated, locked OS thread, binds a TCP listener at addr, accepts a
// single connection, echoes back one line, and closes. It returns a channel
// that receives the result (nil on success) once the exchange completes or
// fails.
//
// The listener is created on the locked thread after entering the
// namespace, so binding succeeds only when addr is actually reachable from
// within that namespace. Combined with an address from attachVeth (which has
// no route from the root namespace), a successful exchange on the returned
// channel is only possible if the connecting peer's traffic actually
// originated inside the sandbox namespace.
func listenAndEchoOnceInNetns(tb testing.TB, nsPath, addr string) <-chan error {
	tb.Helper()

	ready := make(chan error, 1)
	result := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		// Intentionally never unlocked: Go retires this OS thread when the
		// goroutine exits (Go 1.10+), so the namespace change does not leak
		// into the shared thread pool.

		f, err := os.Open(nsPath)
		if err != nil {
			ready <- fmt.Errorf("open netns %q: %w", nsPath, err)
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			ready <- fmt.Errorf("setns %q: %w", nsPath, err)
			return
		}

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			ready <- fmt.Errorf("listen %s in netns: %w", addr, err)
			return
		}
		ready <- nil

		if tcpLn, ok := ln.(*net.TCPListener); ok {
			tcpLn.SetDeadline(time.Now().Add(20 * time.Second))
		}
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			result <- fmt.Errorf("accept: %w", err)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))

		buf := make([]byte, 256)
		n, rerr := conn.Read(buf)
		if n == 0 && rerr != nil {
			result <- fmt.Errorf("read: %w", rerr)
			return
		}
		if _, err := conn.Write(buf[:n]); err != nil {
			result <- fmt.Errorf("write: %w", err)
			return
		}
		result <- nil
	}()

	if err := <-ready; err != nil {
		tb.Fatalf("listenAndEchoOnceInNetns: %v", err)
	}
	return result
}
