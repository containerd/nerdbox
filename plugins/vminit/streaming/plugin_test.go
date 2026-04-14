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

package streaming

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/types/known/anypb"
)

// newTestPair returns a connected pair of vsockStreams for testing.
func newTestPair(t *testing.T) (sender *vsockStream, receiver *vsockStream) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return &vsockStream{conn: a}, &vsockStream{conn: b}
}

// makeAny creates a typeurl.Any wrapping arbitrary bytes for testing.
func makeAny(t *testing.T, data []byte) typeurl.Any {
	t.Helper()
	return &anypb.Any{
		TypeUrl: "test/bytes",
		Value:   data,
	}
}

func TestSendRecvRoundtrip(t *testing.T) {
	sender, receiver := newTestPair(t)

	payload := []byte("hello, stream")
	msg := makeAny(t, payload)

	done := make(chan error, 1)
	go func() {
		done <- sender.Send(msg)
	}()

	got, err := receiver.Recv()
	if err != nil {
		t.Fatal("Recv:", err)
	}
	if err := <-done; err != nil {
		t.Fatal("Send:", err)
	}

	if got.GetTypeUrl() != "test/bytes" {
		t.Fatalf("type URL = %q, want %q", got.GetTypeUrl(), "test/bytes")
	}
	if !bytes.Equal(got.GetValue(), payload) {
		t.Fatalf("value mismatch: got %d bytes, want %d", len(got.GetValue()), len(payload))
	}
}

func TestSendRecvMultipleMessages(t *testing.T) {
	sender, receiver := newTestPair(t)

	const count = 100
	done := make(chan error, 1)
	go func() {
		for i := range count {
			buf := make([]byte, 64)
			buf[0] = byte(i)
			if err := sender.Send(makeAny(t, buf)); err != nil {
				done <- err
				return
			}
		}
		done <- sender.Close()
	}()

	for i := range count {
		got, err := receiver.Recv()
		if err != nil {
			t.Fatalf("Recv[%d]: %v", i, err)
		}
		if got.GetValue()[0] != byte(i) {
			t.Fatalf("message %d: got marker %d", i, got.GetValue()[0])
		}
	}

	// Next recv should be EOF from the Close()
	_, err := receiver.Recv()
	if err != io.EOF {
		t.Fatalf("expected EOF after Close, got: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatal("sender:", err)
	}
}

func TestCloseProducesEOF(t *testing.T) {
	sender, receiver := newTestPair(t)

	done := make(chan error, 1)
	go func() {
		done <- sender.Close()
	}()

	_, err := receiver.Recv()
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal("Close:", err)
	}
}

func TestDoubleCloseIsIdempotent(t *testing.T) {
	sender, receiver := newTestPair(t)

	done := make(chan error, 1)
	go func() {
		err1 := sender.Close()
		err2 := sender.Close() // should be no-op
		if err1 != nil {
			done <- err1
		} else {
			done <- err2
		}
	}()

	// Should get exactly one EOF
	_, err := receiver.Recv()
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatal("Close:", err)
	}
}

func TestSequentialSendThenClose(t *testing.T) {
	// Send and Close are called sequentially from the same goroutine,
	// matching the real usage pattern (SendStream loop then Close).
	sender, receiver := newTestPair(t)

	const count = 200

	done := make(chan error, 1)
	go func() {
		for i := range count {
			data := []byte{byte(i)}
			if err := sender.Send(makeAny(t, data)); err != nil {
				done <- err
				return
			}
		}
		done <- sender.Close()
	}()

	var received int
	for {
		_, err := receiver.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv returned unexpected error after %d messages: %v", received, err)
		}
		received++
	}

	if received != count {
		t.Fatalf("received %d messages, want %d", received, count)
	}
	if err := <-done; err != nil {
		t.Fatal("sender:", err)
	}
}

func TestLargeMessage(t *testing.T) {
	sender, receiver := newTestPair(t)

	// 1 MiB payload
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- sender.Send(makeAny(t, payload))
	}()

	got, err := receiver.Recv()
	if err != nil {
		t.Fatal("Recv:", err)
	}
	if err := <-done; err != nil {
		t.Fatal("Send:", err)
	}
	if !bytes.Equal(got.GetValue(), payload) {
		t.Fatal("large message data mismatch")
	}
}

func TestRecvRejectsOversizedFrame(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})

	receiver := &vsockStream{conn: b}

	// Write a length header that exceeds maxFrameSize
	done := make(chan error, 1)
	go func() {
		done <- binary.Write(a, binary.BigEndian, uint32(maxFrameSize+1))
	}()

	_, err := receiver.Recv()
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
	if err == io.EOF {
		t.Fatal("expected size error, not EOF")
	}
	<-done
}

func TestRecvConnectionClosed(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { b.Close() })
	receiver := &vsockStream{conn: b}

	// Close the writer side — Recv should get an error (not hang)
	a.Close()

	_, err := receiver.Recv()
	if err == nil {
		t.Fatal("expected error on closed connection")
	}
}

func TestSendAfterClose(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})

	sender := &vsockStream{conn: a}
	receiver := &vsockStream{conn: b}

	// Drain receiver in background — keep reading even after EOF
	// to prevent the sender's post-close Send from blocking on pipe.
	go func() {
		for {
			_, err := receiver.Recv()
			if err != nil {
				// Keep draining the raw conn so writes don't block
				io.Copy(io.Discard, b)
				return
			}
		}
	}()

	if err := sender.Close(); err != nil {
		t.Fatal("Close:", err)
	}
	// Send after close should still work at the conn level (Close only
	// sends EOF marker, doesn't close conn). This just ensures no panic.
	_ = sender.Send(makeAny(t, []byte("after-close")))
}

// TestBidirectionalStreaming verifies that both sides can send and receive
// concurrently on the same connection, as happens in real transfer streams
// (one direction carries data, the other carries window updates).
func TestBidirectionalStreaming(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})

	streamA := &vsockStream{conn: a}
	streamB := &vsockStream{conn: b}

	const messages = 50

	// A sends to B, B sends to A — concurrently
	var wg sync.WaitGroup
	wg.Add(4)

	// A -> B sender
	go func() {
		defer wg.Done()
		for i := range messages {
			if err := streamA.Send(makeAny(t, []byte{byte(i)})); err != nil {
				return
			}
		}
		streamA.Close()
	}()

	// B -> A sender
	go func() {
		defer wg.Done()
		for i := range messages {
			if err := streamB.Send(makeAny(t, []byte{byte(i + 128)})); err != nil {
				return
			}
		}
		streamB.Close()
	}()

	// B receives from A
	var countB int
	go func() {
		defer wg.Done()
		for {
			_, err := streamB.Recv()
			if err != nil {
				return
			}
			countB++
		}
	}()

	// A receives from B
	var countA int
	go func() {
		defer wg.Done()
		for {
			_, err := streamA.Recv()
			if err != nil {
				return
			}
			countA++
		}
	}()

	wg.Wait()

	if countB != messages {
		t.Errorf("B received %d messages, want %d", countB, messages)
	}
	if countA != messages {
		t.Errorf("A received %d messages, want %d", countA, messages)
	}
}

// FuzzRecv feeds arbitrary bytes into Recv to check it never panics
// or allocates unbounded memory.
func FuzzRecv(f *testing.F) {
	// Seed with interesting cases
	f.Add([]byte{})                                     // empty
	f.Add([]byte{0, 0, 0, 0})                           // zero-length frame (EOF)
	f.Add([]byte{0, 0, 0, 5, 1, 2, 3, 4, 5})           // valid 5-byte frame
	f.Add([]byte{0, 0, 0, 1, 0xff})                     // 1-byte frame
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})                // max uint32 length
	f.Add([]byte{0, 0, 0, 3, 1, 2})                     // truncated data
	f.Add([]byte{0, 0, 0, 1})                            // length but no data
	f.Add([]byte{0, 0, 0, 5, 10, 5, 116, 101, 115, 116}) // valid proto Any

	// Two frames back to back
	f.Add([]byte{0, 0, 0, 1, 0xAA, 0, 0, 0, 0}) // frame then EOF

	f.Fuzz(func(t *testing.T, data []byte) {
		a, b := net.Pipe()
		t.Cleanup(func() {
			a.Close()
			b.Close()
		})

		receiver := &vsockStream{conn: b}

		// Write all data then close so Recv doesn't block forever
		go func() {
			a.Write(data)
			a.Close()
		}()

		// Drain all messages until error — must not panic or OOM
		for {
			_, err := receiver.Recv()
			if err != nil {
				break
			}
		}
	})
}

// FuzzSendRecv round-trips arbitrary payloads through Send/Recv.
func FuzzSendRecv(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add(make([]byte, 1024))
	f.Add([]byte("hello world"))

	f.Fuzz(func(t *testing.T, payload []byte) {
		sender, receiver := newTestPair(t)

		done := make(chan error, 1)
		go func() {
			err := sender.Send(makeAny(t, payload))
			if err != nil {
				done <- err
				return
			}
			done <- sender.Close()
		}()

		got, err := receiver.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if !bytes.Equal(got.GetValue(), payload) {
			t.Fatalf("payload mismatch: sent %d bytes, got %d", len(payload), len(got.GetValue()))
		}

		// Should get EOF
		_, err = receiver.Recv()
		if err != io.EOF {
			t.Fatalf("expected EOF, got: %v", err)
		}

		if err := <-done; err != nil {
			t.Fatal("sender:", err)
		}
	})
}

// FuzzRecvFrameSequence feeds sequences of well-formed frames with random
// payloads and validates that every message round-trips correctly.
func FuzzRecvFrameSequence(f *testing.F) {
	f.Add(1, 64)
	f.Add(5, 128)
	f.Add(10, 0)
	f.Add(50, 1024)

	f.Fuzz(func(t *testing.T, count int, payloadSize int) {
		if count < 0 || count > 200 {
			return
		}
		if payloadSize < 0 || payloadSize > 64*1024 {
			return
		}

		sender, receiver := newTestPair(t)

		// Generate and send messages
		payloads := make([][]byte, count)
		done := make(chan error, 1)
		go func() {
			for i := range count {
				p := make([]byte, payloadSize)
				if payloadSize > 0 {
					p[0] = byte(i)
					if payloadSize > 1 {
						p[payloadSize-1] = byte(i)
					}
				}
				payloads[i] = p
				if err := sender.Send(makeAny(t, p)); err != nil {
					done <- err
					return
				}
			}
			done <- sender.Close()
		}()

		// Receive and validate
		for i := range count {
			got, err := receiver.Recv()
			if err != nil {
				t.Fatalf("Recv[%d]: %v", i, err)
			}
			if !bytes.Equal(got.GetValue(), payloads[i]) {
				t.Fatalf("message %d payload mismatch", i)
			}
		}

		_, err := receiver.Recv()
		if err != io.EOF {
			t.Fatalf("expected EOF, got: %v", err)
		}

		if err := <-done; err != nil {
			t.Fatal("sender:", err)
		}
	})
}
