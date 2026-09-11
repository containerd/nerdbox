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

package transfer

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
)

// fakeStream stands in for the stream the client established for a
// transfer, recording whether it was released.
type fakeStream struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeStream) Send(typeurl.Any) error { return nil }

func (f *fakeStream) Recv() (typeurl.Any, error) { return nil, io.EOF }

func (f *fakeStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeStream) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// recordingContainerFS stands in for the container filesystem registry.
type recordingContainerFS struct {
	// root is the directory operations run against, in place of the container
	// root a real registry would supply.
	root string
	err  error

	requested []string
}

func (o *recordingContainerFS) Do(containerID string, fn func() error) error {
	o.requested = append(o.requested, containerID)
	if o.err != nil {
		return o.err
	}
	return fn()
}

// TestTransferReportsUnresolvableContainer covers the case that used to be
// silent: a container whose filesystem cannot be reached. Falling back to the
// bundle rootfs would appear to succeed while reading and writing entries the
// container does not see, so the failure has to surface instead.
//
// The stream is established while the request is unmarshalled, before this
// transferrer runs, so it must also be released. Leaving it open strands the
// client waiting on a transfer that has already failed.
func TestTransferReportsUnresolvableContainer(t *testing.T) {
	for _, tc := range []struct {
		name string
		pair func(*fakeStream) (src, dst any)
	}{
		{
			name: "copy-from",
			pair: func(f *fakeStream) (any, any) {
				return &ContainerPath{ContainerID: "ctr", Path: "/etc/resolv.conf"},
					&WriteStream{MediaType: mediaTypeTar, stream: f}
			},
		},
		{
			name: "copy-to",
			pair: func(f *fakeStream) (any, any) {
				return &ReadStream{MediaType: mediaTypeTar, stream: f},
					&ContainerPath{ContainerID: "ctr", Path: "/etc/resolv.conf"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &fakeStream{}
			src, dst := tc.pair(stream)

			ctrFS := &recordingContainerFS{err: errdefs.ErrNotFound}
			tr := NewContainerFSTransferrer(ctrFS)

			err := tr.Transfer(context.Background(), src, dst)
			if !errors.Is(err, errdefs.ErrNotFound) {
				t.Fatalf("Transfer error = %v, want it to wrap ErrNotFound", err)
			}
			if len(ctrFS.requested) != 1 || ctrFS.requested[0] != "ctr" {
				t.Errorf("container filesystem saw %v, want exactly [ctr]", ctrFS.requested)
			}
			if !stream.isClosed() {
				t.Error("the client's stream was left open after the transfer failed")
			}
		})
	}
}

// TestTransferRejectsUnsupportedPairs pins the existing contract that only
// ContainerPath paired with a stream is handled.
func TestTransferRejectsUnsupportedPairs(t *testing.T) {
	ctrFS := &recordingContainerFS{err: errors.New("must not be consulted")}
	tr := NewContainerFSTransferrer(ctrFS)

	for _, tc := range []struct {
		name     string
		src, dst any
	}{
		{"container to container", &ContainerPath{ContainerID: "ctr"}, &ContainerPath{ContainerID: "other"}},
		{"stream to stream", &ReadStream{}, &WriteStream{}},
		{"unknown source", struct{}{}, &WriteStream{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tr.Transfer(context.Background(), tc.src, tc.dst)
			if !errors.Is(err, errdefs.ErrNotImplemented) {
				t.Errorf("Transfer error = %v, want ErrNotImplemented", err)
			}
		})
	}

	if len(ctrFS.requested) != 0 {
		t.Errorf("container filesystem consulted for unsupported pairs: %v", ctrFS.requested)
	}
}
