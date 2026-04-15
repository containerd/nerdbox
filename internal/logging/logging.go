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

// Package logging provides unified structured logging utilities for the
// shim and vminitd components.
package logging

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/containerd/log"
)

// SetupShimLog configures slog-based logging for the shim process.
// It opens the platform-specific log output (FIFO on Unix, named pipe
// on Windows), then creates a slog TextHandler and sets it as the
// default logger with a "component=shim" attribute.
//
// The base handler (without component) is stored for use by
// [ForwardConsoleLogs] so that forwarded records carry their own
// component rather than inheriting "shim".
//
// For the short-lived start and delete actions, only [log.UseSlog] is
// called to route logrus through slog; the log output is not opened.
func SetupShimLog() {
	log.UseSlog()

	var (
		debug bool
		ns    string
		id    string
		attrs []slog.Attr
	)
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "start", "delete":
			return
		case "-debug":
			debug = true
		case "-namespace":
			if i+1 < len(args) {
				i++
				ns = args[i]
				attrs = append(attrs, slog.String("ns", ns))
			}
		case "-id":
			if i+1 < len(args) {
				i++
				id = args[i]
				attrs = append(attrs, slog.String("id", id))
			}
		}
	}

	w := openShimLog(ns, id)

	var level slog.LevelVar
	if debug {
		level.Set(slog.LevelDebug)
		log.SetLevel("debug") //nolint:errcheck
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: &level}).WithAttrs(attrs)
	SetBaseHandler(handler)
	slog.SetDefault(slog.New(handler).With("component", "shim"))
}

// baseHandler is the slog handler used by ForwardConsoleLogs to emit
// records without the caller's pre-applied attributes (e.g. component=shim).
var baseHandler slog.Handler

// SetBaseHandler stores the base handler for use by ForwardConsoleLogs.
// This should be called before any console forwarding starts, typically
// during init with the handler before any .With() attributes are applied.
func SetBaseHandler(h slog.Handler) {
	baseHandler = h
}

// consoleHandler returns the handler that ForwardConsoleLogs should use.
// It prefers the base handler set via SetBaseHandler, falling back to
// the default slog handler.
func consoleHandler() slog.Handler {
	if baseHandler != nil {
		return baseHandler
	}
	return slog.Default().Handler()
}

// ForwardConsoleLogs reads lines from r and re-emits them as structured
// log entries through the base [slog.Handler] set via [SetBaseHandler].
//
// Lines that are valid JSON objects (emitted by vminitd's JSON slog handler)
// are parsed and re-emitted preserving the original level, message,
// and attributes. All other lines are treated as kernel messages and emitted
// at INFO level with component=kmsg.
//
// The base handler is used directly (rather than the default logger) so that
// pre-applied attributes such as component=shim are not added to forwarded
// records, which carry their own component.
//
// If raw is non-nil, every line is also written there verbatim (useful for
// tests that need the unprocessed console output).
func ForwardConsoleLogs(r io.Reader, raw io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()

		if raw != nil {
			raw.Write([]byte(line))
			raw.Write([]byte("\n"))
		}

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "{") {
			if forwardJSONLog(line) {
				continue
			}
		}

		// Kernel message — parse optional "[    1.234567] " timestamp prefix.
		msg := line
		attrs := []slog.Attr{slog.String("component", "kmsg")}
		if after, ktime, ok := parseKernelTimestamp(line); ok {
			msg = after
			attrs = append(attrs, slog.String("ktime", ktime))
		}
		record := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
		record.AddAttrs(attrs...)
		handler := consoleHandler()
		if handler.Enabled(context.Background(), slog.LevelInfo) {
			handler.Handle(context.Background(), record) //nolint:errcheck
		}
	}
	if err := scanner.Err(); err != nil {
		record := slog.NewRecord(time.Now(), slog.LevelWarn, "console log reader stopped", 0)
		record.AddAttrs(slog.String("component", "kmsg"), slog.Any("error", err))
		handler := consoleHandler()
		if handler.Enabled(context.Background(), slog.LevelWarn) {
			handler.Handle(context.Background(), record) //nolint:errcheck
		}
	}
}

// forwardJSONLog attempts to parse line as a JSON slog record and emit it
// through the console handler. Returns true if the line was handled.
func forwardJSONLog(line string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return false
	}

	// A valid vminitd log must at least have "msg".
	rawMsg, ok := fields["msg"]
	if !ok {
		return false
	}

	var msg string
	if err := json.Unmarshal(rawMsg, &msg); err != nil {
		return false
	}
	delete(fields, "msg")

	var level slog.Level
	if raw, ok := fields["level"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			level.UnmarshalText([]byte(s)) //nolint:errcheck
		}
		delete(fields, "level")
	}

	// Discard the VM-side timestamp — the guest clock is not
	// synchronised and typically reads as epoch.
	delete(fields, "time")
	t := time.Now()

	handler := consoleHandler()
	if !handler.Enabled(context.Background(), level) {
		return true
	}

	record := slog.NewRecord(t, level, msg, 0)
	for k, v := range fields {
		var val any
		if err := json.Unmarshal(v, &val); err == nil {
			record.AddAttrs(slog.Any(k, val))
		}
	}

	handler.Handle(context.Background(), record) //nolint:errcheck
	return true
}

// parseKernelTimestamp extracts the "[  seconds.usecs] " prefix from a
// kernel log line. Returns the message after the prefix, the timestamp
// string, and whether a timestamp was found.
func parseKernelTimestamp(line string) (msg, ktime string, ok bool) {
	if len(line) < 3 || line[0] != '[' {
		return "", "", false
	}
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return "", "", false
	}
	ktime = strings.TrimSpace(line[1:end])
	msg = line[end+1:]
	if len(msg) > 0 && msg[0] == ' ' {
		msg = msg[1:]
	}
	return msg, ktime, true
}
