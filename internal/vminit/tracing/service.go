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

package tracing

import (
	"context"

	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"

	tracespb "github.com/containerd/nerdbox/api/services/traces/v1"
)

// Service implements the TTRPCTracesService by streaming spans from an
// Exporter's channel to the host.
type Service struct {
	exporter *Exporter
}

// NewService creates a new traces streaming service.
func NewService(exp *Exporter) *Service {
	return &Service{exporter: exp}
}

// RegisterTTRPC registers the service with the ttrpc server.
func (s *Service) RegisterTTRPC(server *ttrpc.Server) error {
	tracespb.RegisterTTRPCTracesService(server, s)
	return nil
}

// Stream sends spans from the exporter channel to the client.
func (s *Service) Stream(ctx context.Context, _ *emptypb.Empty, ss tracespb.TTRPCTraces_StreamServer) error {
	for {
		select {
		case span := <-s.exporter.Chan():
			if err := ss.Send(span); err != nil {
				return err
			}
		case <-s.exporter.Done():
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
