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

package version

import (
	"context"
	"log/slog"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api "github.com/containerd/nerdbox/api/services/system/v1"
)

// TestSetLogLevel_ValidLevel verifies that valid LogLevel enum values succeed
// and map to the expected logrus level string and slog.Level.
func TestSetLogLevel_ValidLevel(t *testing.T) {
	cases := []struct {
		enum       api.LogLevel
		wantSlog   slog.Level
		wantLogrus string
	}{
		{api.LogLevel_LOG_LEVEL_TRACE, slog.LevelDebug - 4, "trace"},
		{api.LogLevel_LOG_LEVEL_DEBUG, slog.LevelDebug, "debug"},
		{api.LogLevel_LOG_LEVEL_INFO, slog.LevelInfo, "info"},
		{api.LogLevel_LOG_LEVEL_WARN, slog.LevelWarn, "warn"},
		{api.LogLevel_LOG_LEVEL_ERROR, slog.LevelError, "error"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.enum.String(), func(t *testing.T) {
			svc := &service{}
			_, err := svc.SetLogLevel(context.Background(), &api.SetLogLevelRequest{Level: tc.enum})
			if err != nil {
				t.Fatalf("SetLogLevel(%v) unexpected error: %v", tc.enum, err)
			}
			// Exercise the helper directly to verify the mapping.
			gotStr, gotSlog, err := logLevelToStrings(tc.enum)
			if err != nil {
				t.Fatalf("logLevelToStrings(%v) unexpected error: %v", tc.enum, err)
			}
			if gotStr != tc.wantLogrus {
				t.Errorf("logLevelToStrings(%v) logrus = %q, want %q", tc.enum, gotStr, tc.wantLogrus)
			}
			if gotSlog != tc.wantSlog {
				t.Errorf("logLevelToStrings(%v) slog = %v, want %v", tc.enum, gotSlog, tc.wantSlog)
			}
		})
	}
}

// TestSetLogLevel_InvalidLevel verifies that LOG_LEVEL_UNSPECIFIED (0) and
// an out-of-range value both return codes.InvalidArgument.
func TestSetLogLevel_InvalidLevel(t *testing.T) {
	cases := []struct {
		name  string
		level api.LogLevel
	}{
		{"unspecified", api.LogLevel_LOG_LEVEL_UNSPECIFIED},
		{"out-of-range", api.LogLevel(99)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			svc := &service{}
			_, err := svc.SetLogLevel(context.Background(), &api.SetLogLevelRequest{Level: tc.level})
			if err == nil {
				t.Fatalf("SetLogLevel(%v) expected error, got nil", tc.level)
			}
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("error is not a gRPC status: %v", err)
			}
			if got := st.Code(); got != codes.InvalidArgument {
				t.Errorf("gRPC code = %v, want %v", got, codes.InvalidArgument)
			}
		})
	}
}
