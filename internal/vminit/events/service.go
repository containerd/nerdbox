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

package events

import (
	"context"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/dmcgowan/nerdbox/api/services/vmevents/v1"
)

type Subscriber interface {
	Subscribe(context.Context, ...string) (<-chan *events.Envelope, <-chan error)
}

type service struct {
	sub Subscriber
}

func NewService(s Subscriber) *service {
	return &service{
		sub: s,
	}
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	vmevents.RegisterTTRPCEventsService(server, s)
	return nil
}

func (s *service) Stream(ctx context.Context, _ *emptypb.Empty, ss vmevents.TTRPCEvents_StreamServer) error {
	events, errs := s.sub.Subscribe(ctx)
	for {
		select {
		case event := <-events:
			if err := ss.Send(toProto(event)); err != nil {
				return err
			}
		case err := <-errs:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func toProto(env *events.Envelope) *types.Envelope {
	return &types.Envelope{
		Timestamp: protobuf.ToTimestamp(env.Timestamp),
		Namespace: env.Namespace,
		Topic:     env.Topic,
		Event:     typeurl.MarshalProto(env.Event),
	}
}
