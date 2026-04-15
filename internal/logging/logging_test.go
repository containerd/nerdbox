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

package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// discardHandler is a handler that discards all output but still processes records.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler            { return h }

// --- forwardJSONLog benchmarks ---

var sampleJSONLog = `{"time":"2024-01-01T00:00:00.000000000Z","level":"INFO","msg":"starting vminitd","component":"vminitd","args":["--debug"]}`
var sampleJSONLogDebug = `{"time":"2024-01-01T00:00:00.000000000Z","level":"DEBUG","msg":"loaded plugin","component":"vminitd","plugin_id":"io.containerd.task.v1"}`
var sampleJSONLogManyFields = `{"time":"2024-01-01T00:00:00.000000000Z","level":"INFO","msg":"network configured","component":"vminitd","iface":"eth0","ip":"10.0.0.2","gateway":"10.0.0.1","dns":"8.8.8.8","mtu":1500}`

func BenchmarkForwardJSONLog_Typical(b *testing.B) {
	SetBaseHandler(discardHandler{})
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		forwardJSONLog(sampleJSONLog)
	}
}

func BenchmarkForwardJSONLog_ManyFields(b *testing.B) {
	SetBaseHandler(discardHandler{})
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		forwardJSONLog(sampleJSONLogManyFields)
	}
}

func BenchmarkForwardJSONLog_NotJSON(b *testing.B) {
	SetBaseHandler(discardHandler{})
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		forwardJSONLog("{not valid json")
	}
}

// --- ForwardConsoleLogs benchmarks ---

func buildConsoleInput(nKmsg, nJSON int) string {
	var sb strings.Builder
	kmsg := "[    1.234567] virtio_net virtio0 enp0s3: renamed from eth0\n"
	jsonLine := sampleJSONLog + "\n"
	for i := 0; i < nKmsg; i++ {
		sb.WriteString(kmsg)
	}
	for i := 0; i < nJSON; i++ {
		sb.WriteString(jsonLine)
	}
	return sb.String()
}

func BenchmarkForwardConsoleLogs_KmsgOnly(b *testing.B) {
	SetBaseHandler(discardHandler{})
	input := buildConsoleInput(100, 0)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		ForwardConsoleLogs(strings.NewReader(input), nil)
	}
}

func BenchmarkForwardConsoleLogs_JSONOnly(b *testing.B) {
	SetBaseHandler(discardHandler{})
	input := buildConsoleInput(0, 100)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		ForwardConsoleLogs(strings.NewReader(input), nil)
	}
}

func BenchmarkForwardConsoleLogs_Mixed(b *testing.B) {
	SetBaseHandler(discardHandler{})
	input := buildConsoleInput(50, 50)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		ForwardConsoleLogs(strings.NewReader(input), nil)
	}
}

func BenchmarkForwardConsoleLogs_WithRawWriter(b *testing.B) {
	SetBaseHandler(discardHandler{})
	input := buildConsoleInput(50, 50)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		ForwardConsoleLogs(strings.NewReader(input), io.Discard)
	}
}

// --- Correctness tests ---

// collectHandler collects records for inspection.
type collectHandler struct {
	records []slog.Record
}

func (h *collectHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *collectHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *collectHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *collectHandler) WithGroup(string) slog.Handler       { return h }

func TestForwardConsoleLogs_JSONLine(t *testing.T) {
	ch := &collectHandler{}
	SetBaseHandler(ch)

	input := sampleJSONLog + "\n"
	ForwardConsoleLogs(strings.NewReader(input), nil)

	if len(ch.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(ch.records))
	}
	r := ch.records[0]
	if r.Message != "starting vminitd" {
		t.Errorf("unexpected message: %s", r.Message)
	}
	if r.Level != slog.LevelInfo {
		t.Errorf("unexpected level: %s", r.Level)
	}
	var gotComponent string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			gotComponent = a.Value.String()
		}
		return true
	})
	if gotComponent != "vminitd" {
		t.Errorf("expected component=vminitd, got %q", gotComponent)
	}
}

func TestForwardConsoleLogs_KernelLine(t *testing.T) {
	ch := &collectHandler{}
	SetBaseHandler(ch)

	input := "[    1.234567] virtio_net virtio0 enp0s3: renamed from eth0\n"
	ForwardConsoleLogs(strings.NewReader(input), nil)

	if len(ch.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(ch.records))
	}
	r := ch.records[0]
	if r.Level != slog.LevelInfo {
		t.Errorf("unexpected level: %s", r.Level)
	}
	var gotComponent string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			gotComponent = a.Value.String()
		}
		return true
	})
	if gotComponent != "kmsg" {
		t.Errorf("expected component=kmsg, got %q", gotComponent)
	}
}

func TestForwardConsoleLogs_RawWriter(t *testing.T) {
	ch := &collectHandler{}
	SetBaseHandler(ch)

	input := "kernel line\n" + sampleJSONLog + "\n"
	var raw bytes.Buffer
	ForwardConsoleLogs(strings.NewReader(input), &raw)

	if len(ch.records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(ch.records))
	}
	// Raw writer should get both lines verbatim.
	expected := "kernel line\n" + sampleJSONLog + "\n"
	if raw.String() != expected {
		t.Errorf("raw output mismatch:\ngot:  %q\nwant: %q", raw.String(), expected)
	}
}

func TestForwardConsoleLogs_EmptyLines(t *testing.T) {
	ch := &collectHandler{}
	SetBaseHandler(ch)

	input := "\n\nsome message\n\n"
	ForwardConsoleLogs(strings.NewReader(input), nil)

	if len(ch.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(ch.records))
	}
}

func TestForwardConsoleLogs_DebugLevel(t *testing.T) {
	ch := &collectHandler{}
	SetBaseHandler(ch)

	ForwardConsoleLogs(strings.NewReader(sampleJSONLogDebug+"\n"), nil)

	if len(ch.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(ch.records))
	}
	if ch.records[0].Level != slog.LevelDebug {
		t.Errorf("expected DEBUG, got %s", ch.records[0].Level)
	}
}

func TestForwardJSONLog_InvalidJSON(t *testing.T) {
	SetBaseHandler(discardHandler{})
	if forwardJSONLog("{not json") {
		t.Error("expected false for invalid JSON")
	}
}

func TestForwardJSONLog_NoMsg(t *testing.T) {
	SetBaseHandler(discardHandler{})
	if forwardJSONLog(`{"level":"INFO","time":"2024-01-01T00:00:00Z"}`) {
		t.Error("expected false for JSON without msg field")
	}
}
