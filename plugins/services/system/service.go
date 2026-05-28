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
	"fmt"
	"log/slog"
	"os"

	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	api "github.com/containerd/nerdbox/api/services/system/v1"
	"github.com/containerd/nerdbox/plugins"
)

var _ api.TTRPCSystemService = &service{}

// Config holds the system service configuration injected at plugin init time.
// The LogLevel field is set by vminitd's init code so that the SetLogLevel RPC
// can adjust the vminitd log level at runtime without importing the
// Linux-only initd package.
type Config struct {
	// LogLevel is the package-level slog LevelVar used by the vminitd JSON
	// handler. If nil, SetLogLevel only updates the containerd/log level.
	LogLevel *slog.LevelVar
}

// SetLogLevelVar implements the interface recognised by initd's plugin loader
// so that the package-level LogLevel is injected without a direct import.
func (c *Config) SetLogLevelVar(lv *slog.LevelVar) {
	c.LogLevel = lv
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   plugins.TTRPCPlugin,
		ID:     "system",
		Config: &Config{},
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	cfg, _ := ic.Config.(*Config)
	return &service{cfg: cfg}, nil
}

type service struct {
	cfg *Config
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCSystemService(server, s)
	return nil
}

func (s *service) Info(ctx context.Context, _ *emptypb.Empty) (*api.InfoResponse, error) {
	v, err := os.ReadFile("/proc/version")
	if err != nil && !os.IsNotExist(err) {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.InfoResponse{
		Version:       "dev",
		KernelVersion: string(v),
	}, nil
}

// SetLogLevel adjusts the vminitd log level at runtime using the LogLevel enum.
// LOG_LEVEL_UNSPECIFIED (0) and any unknown value are rejected with
// codes.InvalidArgument. Both the slog LevelVar and containerd/log (logrus)
// are updated so that records routed through either path reach the output.
func (s *service) SetLogLevel(_ context.Context, req *api.SetLogLevelRequest) (*emptypb.Empty, error) {
	levelStr, slogLevel, err := logLevelToStrings(req.GetLevel())
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if s.cfg != nil && s.cfg.LogLevel != nil {
		s.cfg.LogLevel.Set(slogLevel)
	}
	// Also update containerd/log (logrus) so that log.G(ctx).Debug/... calls
	// reach the slog hook — logrus gates before the hook fires.
	log.SetLevel(levelStr) //nolint:errcheck
	return &emptypb.Empty{}, nil
}

// logLevelToStrings maps a LogLevel enum value to the logrus level string
// accepted by log.SetLevel and the corresponding slog.Level. Returns an
// errdefs.ErrInvalidArgument-wrapped error for UNSPECIFIED or unknown values.
func logLevelToStrings(l api.LogLevel) (string, slog.Level, error) {
	switch l {
	case api.LogLevel_LOG_LEVEL_TRACE:
		return "trace", slog.LevelDebug - 4, nil
	case api.LogLevel_LOG_LEVEL_DEBUG:
		return "debug", slog.LevelDebug, nil
	case api.LogLevel_LOG_LEVEL_INFO:
		return "info", slog.LevelInfo, nil
	case api.LogLevel_LOG_LEVEL_WARN:
		return "warn", slog.LevelWarn, nil
	case api.LogLevel_LOG_LEVEL_ERROR:
		return "error", slog.LevelError, nil
	default:
		return "", 0, fmt.Errorf("unspecified or unknown log level %v: %w", l, errdefs.ErrInvalidArgument)
	}
}
