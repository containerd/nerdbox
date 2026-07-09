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
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
	case "tickexit":
		cmdTickexit(args)
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

// cmdNC is a minimal netcat-compatible tool supporting three modes:
//
//	nc -U <path>           connect to a unix domain socket
//	nc <host> <port>       connect via TCP
//	nc -u <host> <port>    exchange a single UDP datagram
//
// In all modes data flows verbatim between the network endpoint and stdio,
// matching the behaviour of the standard nc(1) utility:
//   - TCP / unix: bidirectional io.Copy (stdin→socket, socket→stdout).
//   - UDP: one unconnected sendto (stdin→remote) then one recvfrom
//     (remote→stdout).  The socket is unconnected (ListenPacket / WriteTo /
//     ReadFrom, i.e. sendto/recvfrom) so that shim networking layers cannot
//     short-circuit routing based on the local connect(2) call, which for UDP
//     always succeeds regardless of whether any peer is listening.
func cmdNC(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: nc [-u] <host> <port> | nc -U <socket-path>")
		os.Exit(1)
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
