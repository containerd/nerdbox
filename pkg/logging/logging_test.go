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

	"github.com/containerd/log"
)

// discardHandler is a handler that discards all output but still processes records.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

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
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *collectHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *collectHandler) WithGroup(string) slog.Handler      { return h }

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

// --- ShimLogLevel tests ---

// TestSetGetShimLogLevel_RoundTrip verifies that SetShimLogLevel followed by
// GetShimLogLevel returns the value that was set, and that the containerd/log
// (logrus) level is updated in lockstep.
func TestSetGetShimLogLevel_RoundTrip(t *testing.T) {
	// Restore the original levels after the test so we don't pollute other tests.
	orig := shimLogLevel.Level()
	origLogrus := log.GetLevel().String()
	t.Cleanup(func() {
		shimLogLevel.Set(orig)
		_ = log.SetLevel(origLogrus)
	})

	cases := []struct {
		slogLevel    slog.Level
		logrusLevel  string
	}{
		{slog.LevelDebug, "debug"},
		{slog.LevelInfo, "info"},
		{slog.LevelWarn, "warning"},
		{slog.LevelError, "error"},
	}
	for _, tc := range cases {
		SetShimLogLevel(tc.slogLevel)
		if got := GetShimLogLevel(); got != tc.slogLevel {
			t.Errorf("SetShimLogLevel(%v): GetShimLogLevel() = %v, want %v", tc.slogLevel, got, tc.slogLevel)
		}
		if got := log.GetLevel().String(); got != tc.logrusLevel {
			t.Errorf("SetShimLogLevel(%v): log.GetLevel() = %q, want %q", tc.slogLevel, got, tc.logrusLevel)
		}
	}
}

// TestShimLogLevel_HandlerRespectsPostSetupChange verifies that a handler
// wired to the package-level shimLogLevel (as SetupShimLog does) immediately
// reflects level changes made after setup via SetShimLogLevel.
func TestShimLogLevel_HandlerRespectsPostSetupChange(t *testing.T) {
	// Restore global state after the test — shimLogLevel and the logrus level
	// (SetShimLogLevel updates both).
	orig := shimLogLevel.Level()
	origLogrus := log.GetLevel()
	t.Cleanup(func() {
		shimLogLevel.Set(orig)
		_ = log.SetLevel(origLogrus.String())
	})

	// Start at INFO (the zero value / default).
	shimLogLevel.Set(slog.LevelInfo)

	// Build the handler exactly as SetupShimLog does: pass &shimLogLevel as the
	// HandlerOptions.Level. slog.TextHandler.Enabled reads the LevelVar on every
	// call, so changes via SetShimLogLevel take effect immediately.
	h := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: &shimLogLevel})

	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("handler should NOT be enabled for DEBUG at initial INFO level")
	}
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("handler should be enabled for INFO at initial INFO level")
	}

	// Lower the level to DEBUG via SetShimLogLevel — no handler rebuild needed.
	SetShimLogLevel(slog.LevelDebug)

	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("handler should be enabled for DEBUG after SetShimLogLevel(Debug)")
	}

	// Raise the level to WARN.
	SetShimLogLevel(slog.LevelWarn)

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("handler should NOT be enabled for INFO after SetShimLogLevel(Warn)")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("handler should be enabled for WARN after SetShimLogLevel(Warn)")
	}
}
