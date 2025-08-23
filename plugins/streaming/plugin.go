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
	"context"
	"sync"

	"github.com/containerd/containerd/v2/core/streaming"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

func init() {
	registry.Register(&plugin.Registration{
		Type:     plugins.StreamingPlugin,
		ID:       "manager",
		Requires: []plugin.Type{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			sm := &streamManager{
				streams: map[string]*managedStream{},
			}
			return sm, nil
		},
	})
}

type streamManager struct {
	// streams maps name -> stream
	streams map[string]*managedStream

	rwlock sync.RWMutex
}

func (sm *streamManager) Register(ctx context.Context, name string, stream streaming.Stream) error {
	ms := &managedStream{
		Stream:  stream,
		name:    name,
		manager: sm,
	}

	sm.rwlock.Lock()
	defer sm.rwlock.Unlock()
	if _, ok := sm.streams[name]; ok {
		return errdefs.ErrAlreadyExists
	}
	sm.streams[name] = ms

	return nil
}

func (sm *streamManager) Get(ctx context.Context, name string) (streaming.Stream, error) {
	sm.rwlock.RLock()
	defer sm.rwlock.RUnlock()

	stream, ok := sm.streams[name]
	if !ok {
		return nil, errdefs.ErrNotFound
	}

	return stream, nil
}

type managedStream struct {
	streaming.Stream

	name    string
	manager *streamManager
}

func (m *managedStream) Close() error {
	m.manager.rwlock.Lock()
	delete(m.manager.streams, m.name)

	m.manager.rwlock.Unlock()
	return m.Stream.Close()
}
