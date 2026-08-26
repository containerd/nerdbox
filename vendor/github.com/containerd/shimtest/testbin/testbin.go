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

// Package testbin implements the multicall container binary used by shimtest
// suites. It is intentionally stdlib-only so that it can be compiled as a
// fully static linux binary with CGO_ENABLED=0 and no external dependencies.
//
// Callers that want to embed the binary need only call Main:
//
//	package main
//
//	import "github.com/containerd/shimtest/internal/testbin"
//
//	func main() { testbin.Main() }
package testbin

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Main is the entry point for the testbin multicall binary.  It dispatches
// to the appropriate subcommand based on argv[0] (symlink mode) or argv[1]
// (direct invocation as "testbin <cmd>").
func Main() {
	name := filepath.Base(os.Args[0])

	var cmd string
	var args []string
	if name == "testbin" || name == "" {
		if len(os.Args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: testbin <command> [args...]")
			os.Exit(1)
		}
		cmd = os.Args[1]
		args = os.Args[1:]
	} else {
		cmd = name
		args = os.Args
	}

	switch cmd {
	case "forever":
		cmdForever(args)
	case "burstexit":
		cmdBurstexit(args)
	case "cat":
		cmdCat(args)
	case "date":
		cmdDate(args)
	case "echo":
		cmdEcho(args)
	case "exit":
		cmdExit(args)
	case "hashverify":
		cmdHashverify(args)
	case "layercheck":
		cmdLayercheck(args)
	case "ls":
		cmdLs(args)
	case "memhog":
		cmdMemhog(args)
	case "nc":
		cmdNC(args)
	case "host":
		cmdHost(args)
	case "looptest":
		cmdLooptest(args)
	case "echosrv":
		cmdEchoServer(args)
	case "tickexit":
		cmdTickexit(args)
	case "pidscan":
		cmdPidscan(args)
	case "shmwrite":
		cmdShmWrite(args)
	case "shmread":
		cmdShmRead(args)
	case "shmmapwrite":
		cmdShmMapWrite(args)
	case "shmmapread":
		cmdShmMapRead(args)
	case "hostname":
		cmdHostname(args)
	default:
		fmt.Fprintf(os.Stderr, "testbin: unknown command: %s\n", cmd)
		os.Exit(127)
	}
}

// cmdForever prints its arguments to stdout then blocks forever.
func cmdForever(args []string) {
	if len(args) > 1 {
		fmt.Println(strings.Join(args[1:], " "))
	}
	// Wait for a signal that will never arrive voluntarily.
	// The process will be killed by the test harness.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig)
	<-sig
}

// cmdEcho prints its arguments to stdout and exits.
func cmdEcho(args []string) {
	fmt.Println(strings.Join(args[1:], " "))
}

// cmdCat copies files (or stdin) to stdout.
func cmdCat(args []string) {
	files := args[1:]
	if len(files) == 0 {
		files = []string{"-"}
	}
	for _, name := range files {
		var r io.Reader
		if name == "-" {
			r = os.Stdin
		} else {
			f, err := os.Open(name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cat: %s: %v\n", name, err)
				os.Exit(1)
			}
			defer f.Close()
			r = f
		}
		if _, err := io.Copy(os.Stdout, r); err != nil {
			os.Exit(1)
		}
	}
}

// cmdDate prints the current time. Supports +%s and +%s%N.
func cmdDate(args []string) {
	format := "+%s"
	if len(args) > 1 {
		format = args[1]
	}
	now := time.Now()
	switch format {
	case "+%s":
		fmt.Println(now.Unix())
	case "+%s%N":
		fmt.Printf("%d%09d\n", now.Unix(), now.Nanosecond())
	default:
		fmt.Fprintf(os.Stderr, "date: unsupported format: %s\n", format)
		os.Exit(1)
	}
}

// cmdHashverify reads a file in 1 MiB chunks and verifies the data
// against an expected crc32-Castagnoli (hex). Reads happen on the main
// goroutine using a sync.Pool of buffers; chunks are handed to a
// hashing consumer via a small buffered channel. A non-blocking send
// is tried first and the count of blocking falls is reported, so the
// caller can see when the hash consumer (rather than IO) is the
// bottleneck. On success it prints
//
//	ok bytes=N ns=M cpu_bound=K
//
// to stdout. Hash mismatch or read errors exit non-zero.
//
// Usage: hashverify <file> <expected-hex>
func cmdHashverify(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: hashverify <file> <expected-hex>")
		os.Exit(1)
	}
	path := args[1]
	wantHex := args[2]

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hashverify: open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()

	const bufSize = 1 << 20 // 1 MiB
	pool := &sync.Pool{New: func() any {
		b := make([]byte, bufSize)
		return &b
	}}
	ch := make(chan *[]byte, 8)

	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	var consumerWG sync.WaitGroup
	consumerWG.Add(1)
	go func() {
		defer consumerWG.Done()
		for buf := range ch {
			h.Write(*buf)
			full := (*buf)[:cap(*buf)]
			pool.Put(&full)
		}
	}()

	var (
		total    int64
		cpuBound int
	)
	start := time.Now()
	for {
		bp := pool.Get().(*[]byte)
		full := (*bp)[:cap(*bp)]
		n, rerr := f.Read(full)
		if n > 0 {
			chunk := full[:n]
			cp := &chunk
			select {
			case ch <- cp:
			default:
				cpuBound++
				ch <- cp
			}
			total += int64(n)
		} else {
			pool.Put(bp)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			close(ch)
			fmt.Fprintf(os.Stderr, "hashverify: read %s: %v\n", path, rerr)
			os.Exit(1)
		}
	}
	elapsed := time.Since(start)
	close(ch)
	consumerWG.Wait()

	gotHex := fmt.Sprintf("%08x", h.Sum32())
	if gotHex != wantHex {
		fmt.Fprintf(os.Stderr, "hashverify: hash mismatch for %s: got %s, want %s\n", path, gotHex, wantHex)
		os.Exit(1)
	}

	fmt.Printf("ok bytes=%d ns=%d cpu_bound=%d\n", total, elapsed.Nanoseconds(), cpuBound)
}

// cmdLayercheck verifies the contents of a layered overlay rootfs
// produced by the shimtest LayersSuite test fixtures.
//
// Usage: layercheck <addedDir> <addedCount> <baseDir> <baseCount>
//
// Verifies that:
//   - <addedDir>/file_K exists and contains "layer K\n" for K in 1..addedCount
//   - <baseDir>/base_J does not exist for J in 0..baseCount-1
//   - <baseDir> exists and is empty (no leftover entries)
//
// On success prints "ok added=<n> base_missing=<m>" to stdout. On
// any mismatch it prints diagnostic lines to stderr and exits 1.
// Argument parse errors exit 2.
func cmdLayercheck(args []string) {
	if len(args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: layercheck <addedDir> <addedCount> <baseDir> <baseCount>")
		os.Exit(2)
	}
	addedDir := args[1]
	addedCount, err := strconv.Atoi(args[2])
	if err != nil || addedCount < 0 {
		fmt.Fprintf(os.Stderr, "layercheck: invalid addedCount %q\n", args[2])
		os.Exit(2)
	}
	baseDir := args[3]
	baseCount, err := strconv.Atoi(args[4])
	if err != nil || baseCount < 0 {
		fmt.Fprintf(os.Stderr, "layercheck: invalid baseCount %q\n", args[4])
		os.Exit(2)
	}

	failures := 0

	// Verify each added file is present with the expected content.
	for i := 1; i <= addedCount; i++ {
		path := filepath.Join(addedDir, fmt.Sprintf("file_%d", i))
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "missing added: %s: %v\n", path, err)
			failures++
			continue
		}
		want := fmt.Sprintf("layer %d\n", i)
		if string(data) != want {
			fmt.Fprintf(os.Stderr, "content mismatch %s: got %q, want %q\n", path, string(data), want)
			failures++
		}
	}

	// Verify each base file is absent.
	for j := 0; j < baseCount; j++ {
		path := filepath.Join(baseDir, fmt.Sprintf("base_%d", j))
		_, err := os.Lstat(path)
		if err == nil {
			fmt.Fprintf(os.Stderr, "base file still present: %s\n", path)
			failures++
			continue
		}
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "stat %s: %v\n", path, err)
			failures++
		}
	}

	// Verify base dir exists and contains no leftover entries.
	if entries, err := os.ReadDir(baseDir); err != nil {
		fmt.Fprintf(os.Stderr, "readdir %s: %v\n", baseDir, err)
		failures++
	} else if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Fprintf(os.Stderr, "base dir %s not empty: %v\n", baseDir, names)
		failures++
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "layercheck: %d failure(s)\n", failures)
		os.Exit(1)
	}

	fmt.Printf("ok added=%d base_missing=%d\n", addedCount, baseCount)
}

// cmdLs lists directory contents, printing one entry name per line.
// Usage: ls [<dir>...]
// Exits 1 if any directory cannot be read.
func cmdLs(args []string) {
	dirs := args[1:]
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	exitCode := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ls: %s: %v\n", dir, err)
			exitCode = 1
			continue
		}
		for _, e := range entries {
			fmt.Println(e.Name())
		}
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// cmdExit parses its first argument as an integer status and exits
// with it. Exits 0 if no argument is supplied.
func cmdExit(args []string) {
	if len(args) < 2 {
		os.Exit(0)
	}
	code, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "exit: invalid status %q: %v\n", args[1], err)
		os.Exit(2)
	}
	os.Exit(code)
}

// cmdMemhog allocates memory 1MiB at a time, touching every page to
// force commit, until the kernel OOM-kills the process. Used to drive
// a memory-limited container to its limit.
func cmdMemhog(_ []string) {
	pageSize := os.Getpagesize()
	const chunkSize = 1 << 20 // 1 MiB
	var keep [][]byte
	for {
		b := make([]byte, chunkSize)
		for i := 0; i < chunkSize; i += pageSize {
			b[i] = 0xff
		}
		keep = append(keep, b)
	}
}

// cmdTickexit writes "tick N\n" for N=1..50 with a 1ms delay between
// writes, then exits with status 7. Used to verify that output
// produced up to the moment of exit is captured by the shim.
func cmdTickexit(_ []string) {
	for i := 1; i <= 50; i++ {
		fmt.Printf("tick %d\n", i)
		time.Sleep(1 * time.Millisecond)
	}
	os.Exit(7)
}

// infiniteTileReader emits an infinite repeating 0x00..0xff tile stream.
// Wrap with io.LimitReader to produce a deterministic payload of exactly
// n bytes without allocating the full buffer.
type infiniteTileReader struct{ off int64 }

func (r *infiniteTileReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((r.off + int64(i)) % 256)
	}
	r.off += int64(len(p))
	return len(p), nil
}

// cmdBurstexit writes a deterministic byte stream of the requested
// size to stdout as fast as possible, then exits immediately. The
// stream is a repeating 0x00..0xff tile so that any truncation or
// corruption is detectable by length or CRC-32 checks.
//
// Usage: burstexit <size_bytes> [exit_code]
//
// This is used by the FastExitOutput and FastExitInit tests to expose
// the close-before-drain race in the shim's IO cleanup path: the
// process exits while bytes are still in-flight, and a shim that
// closes stream connections before the goroutines drain will truncate
// the output.
func cmdBurstexit(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: burstexit <size_bytes> [exit_code]")
		os.Exit(1)
	}
	size, err := strconv.Atoi(args[1])
	if err != nil || size < 0 {
		fmt.Fprintf(os.Stderr, "burstexit: invalid size %q\n", args[1])
		os.Exit(1)
	}
	exitCode := 0
	if len(args) >= 3 {
		exitCode, err = strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "burstexit: invalid exit code %q\n", args[2])
			os.Exit(1)
		}
	}

	if _, err := io.Copy(os.Stdout, io.LimitReader(&infiniteTileReader{}, int64(size))); err != nil {
		os.Exit(1)
	}
	os.Exit(exitCode)
}

// cmdNC is a minimal netcat-compatible tool supporting four modes:
//
//	nc -U <path>           connect to a unix domain socket
//	nc [-v] -l <port>      listen on TCP (all IPv4 interfaces), accept one
//	                       connection, then pipe stdio
//	nc <host> <port>       connect via TCP
//	nc -u <host> <port>    exchange a single UDP datagram
//
// In stream modes (TCP / unix) data flows verbatim between the network
// endpoint and stdio, matching the behaviour of the standard nc(1) utility:
// stdout carries connection payload and nothing else.
//
// Listen mode (nc -l): binds tcp4 0.0.0.0:<port> (port 0 = ephemeral), then
// accepts one connection and enters the same bidirectional stdio↔socket pipe
// as connect mode. Listening is restricted to IPv4 (unlike connect mode,
// which is dual-stack) to keep the address a peer must dial unambiguous.
//
// As with standard nc, the listening socket is reported only when -v is
// given, and then on stderr -- never on stdout, which would corrupt the
// payload stream. The line format matches nc -v exactly:
//
//	Listening on 0.0.0.0 39117
//
// It is written before the accept(2) call, so a caller that waits for it can
// use it both to learn an ephemeral port and as a readiness signal. Note
// that -v is honoured for listen mode only; standard nc is also verbose
// about outgoing connections, which is not implemented here.
//
// nc's stream modes are a symmetric bidirectional pipe that only terminates
// once *both* copy directions are done: the peer has closed the connection
// *and* stdin has reached EOF. This matches standard nc, which likewise
// keeps running after a peer close so that a half-closed connection can
// still be written to. A caller driving nc through a shim must therefore
// issue Task.CloseIO to signal stdin EOF; closing its own write end of the
// stdin FIFO is not sufficient. For "one container listens, another
// connects" scenarios where neither end is a host-side Go program able to
// explicitly Close() once done, see echosrv, a purpose-built one-shot
// responder that always terminates on its own.
//
// UDP mode: one unconnected sendto (stdin→remote) then one recvfrom
// (remote→stdout). The socket is unconnected (ListenPacket / WriteTo /
// ReadFrom, i.e. sendto/recvfrom) so that shim networking layers cannot
// short-circuit routing based on the local connect(2) call, which for UDP
// always succeeds regardless of whether any peer is listening.
func cmdNC(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: nc [-u] <host> <port> | nc [-v] -l <port> | nc -U <socket-path>")
		os.Exit(1)
	}

	// Standard nc is silent unless -v is given. Strip it here so the mode
	// dispatch below sees the same argument shape either way.
	verbose := false
	if args[1] == "-v" {
		verbose = true
		args = append([]string{args[0]}, args[2:]...)
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: nc [-v] -l <port>")
			os.Exit(1)
		}
	}

	switch args[1] {
	case "-U":
		// Unix domain socket mode.
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: nc -U <socket-path>")
			os.Exit(1)
		}
		conn, err := net.Dial("unix", args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: %s: %v\n", args[2], err)
			os.Exit(1)
		}
		defer conn.Close()
		ncStream(conn)

	case "-l":
		// Listen mode: nc [-v] -l <port>
		// Binds tcp4 0.0.0.0:<port> (0 = ephemeral), accepts one connection,
		// then bidirectionally pipes stdio↔socket. See the function comment
		// for the -v listen notice and for the termination conditions.
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: nc [-v] -l <port>")
			os.Exit(1)
		}
		ln, err := net.Listen("tcp4", "0.0.0.0:"+args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: listen 0.0.0.0:%s: %v\n", args[2], err)
			os.Exit(1)
		}
		if verbose {
			// Report the socket actually bound, so a caller need not assume
			// the kernel honoured the requested port. os.Stderr is
			// unbuffered, so this reaches the caller before the accept(2)
			// below and is therefore usable as a readiness signal.
			host, boundPort, err := net.SplitHostPort(ln.Addr().String())
			if err != nil {
				fmt.Fprintf(os.Stderr, "nc: splithost: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Listening on %s %s\n", host, boundPort)
		}
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: accept: %v\n", err)
			os.Exit(1)
		}
		defer conn.Close()
		ncStream(conn)

	case "-u":
		// UDP datagram mode.
		if len(args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: nc -u <host> <port>")
			os.Exit(1)
		}
		raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(args[2], args[3]))
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: resolve %s:%s: %v\n", args[2], args[3], err)
			os.Exit(1)
		}
		pc, err := net.ListenPacket("udp", ":0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: udp listen: %v\n", err)
			os.Exit(1)
		}
		defer pc.Close()

		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: read stdin: %v\n", err)
			os.Exit(1)
		}
		if _, err := pc.WriteTo(payload, raddr); err != nil {
			fmt.Fprintf(os.Stderr, "nc: udp send: %v\n", err)
			os.Exit(1)
		}
		// A deadline here is deliberately not a retry: the caller (see
		// attachContainerNetwork) is responsible for the container's
		// network being fully ready before this process ever runs, so a
		// reply that doesn't show up within the deadline is a real
		// failure, not something to wait out. Its only job is to turn a
		// missing reply into a fast, legible error instead of blocking
		// forever.
		pc.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 65536)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: udp recv: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(buf[:n])

	default:
		// TCP mode: nc <host> <port>
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: nc <host> <port>")
			os.Exit(1)
		}
		conn, err := net.Dial("tcp", net.JoinHostPort(args[1], args[2]))
		if err != nil {
			fmt.Fprintf(os.Stderr, "nc: %s:%s: %v\n", args[1], args[2], err)
			os.Exit(1)
		}
		defer conn.Close()
		ncStream(conn)
	}
}

// ncStream copies bidirectionally between conn and stdio, mirroring the
// behaviour of nc(1) in stream (TCP / unix) mode: stdin is forwarded to the
// connection and the connection's output is forwarded to stdout.  Both
// directions run concurrently; ncStream returns when both copies have
// finished.
func ncStream(conn net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(os.Stdout, conn)
	}()
	go func() {
		defer wg.Done()
		io.Copy(conn, os.Stdin)
	}()
	wg.Wait()
}

// cmdHost looks up the IP addresses for a hostname and prints them in the
// format used by the standard host(1) utility from bind-utils:
//
//	<hostname> has address <ip>
//
// one line per address.  Only A/AAAA records are printed; the tool does not
// perform reverse lookups or print NS/MX records.
//
// Usage: host <hostname>
//
// Exits 0 on success. Exits 1 with a diagnostic on stderr if resolution
// fails.
func cmdHost(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: host <hostname>")
		os.Exit(1)
	}
	name := args[1]
	addrs, err := net.LookupHost(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "host: %s: %v\n", name, err)
		os.Exit(1)
	}
	for _, a := range addrs {
		fmt.Printf("%s has address %s\n", name, a)
	}
}

// looptestPortRangeStart and looptestPortRangeEnd bound the candidates
// cmdLooptest picks its listener's port from, instead of asking the guest
// kernel for an ephemeral one (port 0). Some shims proxy each socket call
// to the host independently of the others rather than truly sharing a
// single network stack; under such a shim, a port-0 bind lets each side
// independently pick "an ephemeral port," with no guarantee the two agree
// -- the listener and the connector could each resolve to a different
// number and never actually rendezvous. Binding a concrete port removes
// that ambiguity: both ends of the same in-process test necessarily agree
// on the number, because there is only one port to have picked. The range
// is deliberately outside Linux's default ephemeral range (typically
// 32768-60999), to reduce the chance of colliding with an unrelated
// connection's OS-assigned source port. A short retry loop, rather than a
// single fixed port, absorbs the (small, and this being an in-container
// listener, more theoretical than practical) chance of a collision with
// another process already using a given candidate.
const (
	looptestPortRangeStart = 20000
	looptestPortRangeEnd   = 29999
	looptestBindAttempts   = 20
)

// cmdLooptest verifies in-container loopback connectivity by starting an
// echo listener inside the same process, connecting to it over
// 127.0.0.1, sending a token, and printing the echo to stdout.
//
// It is a self-contained in-process test that does not fork subprocesses: it
// implements a minimal echo server directly, running it on a goroutine. This
// avoids exec dependencies on the container's filesystem while keeping the
// test agnostic to PID-namespace configuration.
//
// Usage: looptest <token>
//
// Exits 0 and prints the echoed token on success. Exits 1 with a diagnostic
// on stderr if the listener, connection, or echo fails.
func cmdLooptest(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: looptest <token>")
		os.Exit(1)
	}
	token := args[1]

	// See looptestPortRangeStart's doc for why this binds a concrete port
	// from a fixed range, with a short retry loop, rather than port 0.
	var ln net.Listener
	var lastErr error
	for range looptestBindAttempts {
		port := looptestPortRangeStart + rand.IntN(looptestPortRangeEnd-looptestPortRangeStart+1)
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			lastErr = err
			continue
		}
		ln = l
		break
	}
	if ln == nil {
		fmt.Fprintf(os.Stderr, "looptest: listen: %v\n", lastErr)
		os.Exit(1)
	}
	_, boundPort, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "looptest: splithost: %v\n", err)
		os.Exit(1)
	}

	// Run the echo server on a goroutine.
	srvDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			srvDone <- fmt.Errorf("accept: %w", err)
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, rerr := conn.Read(buf)
		if n == 0 && rerr != nil {
			srvDone <- fmt.Errorf("read: %w", rerr)
			return
		}
		if _, err := conn.Write(buf[:n]); err != nil {
			srvDone <- fmt.Errorf("write: %w", err)
			return
		}
		srvDone <- nil
	}()

	// Dial back over loopback.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", boundPort), 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "looptest: dial 127.0.0.1:%s: %v\n", boundPort, err)
		os.Exit(1)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte(token)); err != nil {
		fmt.Fprintf(os.Stderr, "looptest: write: %v\n", err)
		os.Exit(1)
	}

	buf := make([]byte, 4096)
	n, rerr := conn.Read(buf)
	conn.Close()
	if n == 0 && rerr != nil {
		fmt.Fprintf(os.Stderr, "looptest: read echo: %v\n", rerr)
		os.Exit(1)
	}
	got := string(buf[:n])
	if got != token {
		fmt.Fprintf(os.Stderr, "looptest: echo mismatch: got %q, want %q\n", got, token)
		os.Exit(1)
	}
	fmt.Println(got)

	if err := <-srvDone; err != nil {
		fmt.Fprintf(os.Stderr, "looptest: server: %v\n", err)
		os.Exit(1)
	}
}

// cmdEchoServer listens on TCP port <port> (all interfaces), accepts exactly
// one connection, reads exactly one chunk of data (up to 4096 bytes), writes
// the same bytes back verbatim, closes the connection, and exits 0.
//
// Unlike "nc -l", which is a general-purpose bidirectional stream pipe (and
// so never closes the connection on its own — the peer must close it),
// echosrv is purpose-built as a one-shot round-trip responder: it always
// terminates on its own once one exchange completes, which is what makes it
// usable as a container's main process in a test that needs the container to
// exit cleanly after proving connectivity (e.g. two containers exchanging
// data over a shared network namespace, where neither side is a host-side Go
// program that can explicitly Close() to signal completion).
//
// Usage: echosrv <port>
func cmdEchoServer(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: echosrv <port>")
		os.Exit(1)
	}
	// tcp4/0.0.0.0 explicitly, not "tcp"/":<port>" (which defaults to a
	// dual-stack IPv6 socket on Linux): shimtest does not assume a shim's
	// default container networking path supports IPv6, only IPv4.
	ln, err := net.Listen("tcp4", "0.0.0.0:"+args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "echosrv: listen 0.0.0.0:%s: %v\n", args[1], err)
		os.Exit(1)
	}
	// Always print the actual bound port before accepting, so the test can
	// discover it even when port 0 (ephemeral) was requested.
	_, boundPort, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "echosrv: splithost: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(boundPort)
	conn, err := ln.Accept()
	ln.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "echosrv: accept: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if n == 0 && err != nil {
		fmt.Fprintf(os.Stderr, "echosrv: read: %v\n", err)
		os.Exit(1)
	}
	if _, err := conn.Write(buf[:n]); err != nil {
		fmt.Fprintf(os.Stderr, "echosrv: write: %v\n", err)
		os.Exit(1)
	}
}

// cmdPidscan lists every PID visible in this process's PID namespace
// along with its cmdline, by scanning /proc. Used by shimtest to verify
// PID namespace sharing across member containers: the test does not
// know the PID number of the sentinel process it is looking for ahead
// of time (only a unique marker string baked into that process's
// argv), so it scans every visible PID's cmdline rather than checking
// one specific PID.
func cmdPidscan(_ []string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pidscan: readdir /proc: %v\n", err)
		os.Exit(1)
	}
	for _, e := range entries {
		name := e.Name()
		if _, err := strconv.Atoi(name); err != nil {
			continue // not a PID directory
		}
		data, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil {
			// The process may have exited between the readdir and this
			// read; that race is expected and not an error.
			continue
		}
		cmdline := strings.ReplaceAll(strings.TrimRight(string(data), "\x00"), "\x00", " ")
		fmt.Printf("%s %s\n", name, cmdline)
	}
}

const (
	shmSize = 4096
	// ipcCreat is IPC_CREAT, from linux/ipc.h. The stdlib syscall
	// package exposes SysV shm's syscall numbers (SYS_SHMGET etc.) but
	// not its flag constants, so this is hardcoded.
	ipcCreat = 0o1000
)

// cmdShmWrite creates (or reuses) a SysV shared memory segment
// identified by a fixed numeric key and writes a marker string into it,
// then detaches — but does not remove — the segment, leaving it behind
// for a later shmread call to find.
//
// Used by shimtest to verify IPC namespace sharing: SysV IPC objects
// are keyed and visible only within the creating process's IPC
// namespace, independent of mount namespace or any bind-mounted
// /dev/shm, so a successful cross-container shmwrite/shmread round
// trip through the same key is conclusive proof of a shared IPC
// namespace (and not, for instance, an artifact of a shared /dev/shm
// bind mount).
//
// Usage: shmwrite <key> <marker>
func cmdShmWrite(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: shmwrite <key> <marker>")
		os.Exit(1)
	}
	key, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shmwrite: invalid key %q: %v\n", args[1], err)
		os.Exit(1)
	}
	marker := args[2]
	if len(marker) >= shmSize {
		fmt.Fprintln(os.Stderr, "shmwrite: marker too large")
		os.Exit(1)
	}

	shmid, _, errno := syscall.Syscall(syscall.SYS_SHMGET, uintptr(key), shmSize, ipcCreat|0600)
	if errno != 0 {
		fmt.Fprintf(os.Stderr, "shmwrite: shmget: %v\n", errno)
		os.Exit(1)
	}
	addr, _, errno := syscall.Syscall(syscall.SYS_SHMAT, shmid, 0, 0)
	if errno != 0 {
		fmt.Fprintf(os.Stderr, "shmwrite: shmat: %v\n", errno)
		os.Exit(1)
	}
	// addr is a raw address returned by the shmat(2) syscall, not derived
	// from a Go pointer, so it doesn't fit vet's recognized safe-conversion
	// patterns even though the conversion itself is valid here.
	buf := (*[shmSize]byte)(unsafe.Pointer(addr)) //nolint:govet
	n := copy(buf[:], marker)
	buf[n] = 0
	syscall.Syscall(syscall.SYS_SHMDT, addr, 0, 0) //nolint:errcheck

	fmt.Println("shmwrite: ok")
}

// cmdShmRead attaches to an existing SysV shared memory segment
// identified by a fixed numeric key (created by a prior shmwrite call,
// possibly in a different container) and prints the marker string
// found in it.
//
// It deliberately omits IPC_CREAT: if the segment does not already
// exist in this process's IPC namespace, that is exactly the "not
// shared" case and must be reported as a failure, rather than silently
// creating a fresh, empty segment that would make a broken test look
// like it passed.
//
// Usage: shmread <key>
func cmdShmRead(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: shmread <key>")
		os.Exit(1)
	}
	key, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shmread: invalid key %q: %v\n", args[1], err)
		os.Exit(1)
	}

	shmid, _, errno := syscall.Syscall(syscall.SYS_SHMGET, uintptr(key), shmSize, 0600)
	if errno != 0 {
		fmt.Println("shmread: NOTFOUND")
		os.Exit(1)
	}
	addr, _, errno := syscall.Syscall(syscall.SYS_SHMAT, shmid, 0, 0)
	if errno != 0 {
		fmt.Fprintf(os.Stderr, "shmread: shmat: %v\n", errno)
		os.Exit(1)
	}
	// See the matching comment in cmdShmWrite: addr comes from shmat(2),
	// not from a Go pointer, so vet can't recognize this as a safe
	// conversion even though it is one.
	buf := (*[shmSize]byte)(unsafe.Pointer(addr)) //nolint:govet
	end := bytes.IndexByte(buf[:], 0)
	if end < 0 {
		end = shmSize
	}
	fmt.Println(string(buf[:end]))
	syscall.Syscall(syscall.SYS_SHMDT, addr, 0, 0) //nolint:errcheck
}

// shmMapSize is the file/mapping size used by cmdShmMapWrite and
// cmdShmMapRead. Independent of shmSize (the SysV segment size above):
// these test a different sharing mechanism and there's no reason to couple
// their sizes.
const shmMapSize = 4096

// cmdShmMapWrite creates (or truncates) the file at path to shmMapSize and
// writes marker into it through an mmap(MAP_SHARED) mapping — not via a
// write(2) call — then unmaps and exits, leaving the file (and, since the
// mapping is MAP_SHARED, its written contents) behind for a later
// shmmapread call to find.
//
// This, together with shmmapread, is a POSIX-shared-memory-style access
// pattern layered on an ordinary file (as, for example, glibc's
// shm_open()+mmap() is): the file's path is what a caller controls to
// target /dev/shm specifically or any other shared location, but the
// read/write path deliberately goes through the mapping, not the file
// descriptor, since what's under test is whether two independent
// mmap(MAP_SHARED) calls on the same underlying file — potentially made by
// processes in different containers, each reaching the file through its
// own bind mount of a shared directory — actually share memory, rather
// than each seeing an independent, disconnected copy.
//
// Usage: shmmapwrite <path> <marker>
func cmdShmMapWrite(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: shmmapwrite <path> <marker>")
		os.Exit(1)
	}
	path := args[1]
	marker := args[2]
	if len(marker) >= shmMapSize {
		fmt.Println("shmmapwrite: marker too large")
		os.Exit(1)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Printf("shmmapwrite: open: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	if err := f.Truncate(shmMapSize); err != nil {
		fmt.Printf("shmmapwrite: truncate: %v\n", err)
		os.Exit(1)
	}

	data, err := syscall.Mmap(int(f.Fd()), 0, shmMapSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		fmt.Printf("shmmapwrite: mmap: %v\n", err)
		os.Exit(1)
	}

	n := copy(data, marker)
	data[n] = 0

	if err := syscall.Munmap(data); err != nil {
		fmt.Printf("shmmapwrite: munmap: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("shmmapwrite: ok")
}

// cmdShmMapRead opens the file at path (created by a prior shmmapwrite
// call, possibly in a different container) and reads the marker string
// back through an mmap(MAP_SHARED) mapping — not via a read(2) call. See
// cmdShmMapWrite for why the access pattern matters.
//
// It deliberately does not create path if missing: a missing file is
// exactly the "not shared" case and must be reported as a failure, rather
// than silently creating a fresh, empty one that would make a broken test
// look like it passed.
//
// Usage: shmmapread <path>
func cmdShmMapRead(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: shmmapread <path>")
		os.Exit(1)
	}
	path := args[1]

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		fmt.Println("shmmapread: NOTFOUND")
		os.Exit(1)
	}
	defer f.Close()

	data, err := syscall.Mmap(int(f.Fd()), 0, shmMapSize, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shmmapread: mmap: %v\n", err)
		os.Exit(1)
	}
	defer syscall.Munmap(data) //nolint:errcheck

	end := bytes.IndexByte(data, 0)
	if end < 0 {
		end = shmMapSize
	}
	fmt.Println(string(data[:end]))
}

// cmdHostname mirrors the standard "hostname" utility's CLI: with no
// argument it prints the calling process's UTS namespace hostname as
// reported by the kernel (via gethostname(2), not /etc/hostname or an
// env var); with one argument it sets the hostname (via sethostname(2))
// and, matching the standard utility, exits immediately and silently on
// success rather than staying running or printing anything.
//
// sethostname(2) requires CAP_SYS_ADMIN in the user namespace that owns
// the target UTS namespace; the container's OCI spec must request that
// capability explicitly (shimtest's base spec grants none) for a set to
// succeed at all. On failure (of either form) a "hostname: ..." message
// is printed to stderr and the process exits non-zero, matching the
// standard utility's error convention.
//
// Because this command exits immediately after a successful set rather
// than holding the UTS namespace open itself, a caller that verifies a
// hostname change is later visible to a different process is also
// proving that the namespace — and its hostname — outlives the process
// that set it, not merely that a still-running setter's own namespace
// is visible.
//
// Usage: hostname [name]
func cmdHostname(args []string) {
	if len(args) < 2 {
		name, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hostname: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(name)
		return
	}

	if err := syscall.Sethostname([]byte(args[1])); err != nil {
		fmt.Fprintf(os.Stderr, "hostname: %v\n", err)
		os.Exit(1)
	}
}
