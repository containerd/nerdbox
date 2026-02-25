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
	"io"

	ctransfer "github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/errdefs"
)

// NewEchoTransferrer returns a Transferrer that copies data from a
// ReadStream source to a WriteStream destination. This is useful for
// testing the streaming pipeline end-to-end.
func NewEchoTransferrer() ctransfer.Transferrer {
	return &echoTransferrer{}
}

type echoTransferrer struct{}

func (t *echoTransferrer) Transfer(ctx context.Context, src, dst any, opts ...ctransfer.Opt) error {
	s, ok := src.(*ReadStream)
	if !ok {
		return errdefs.ErrNotImplemented
	}
	d, ok := dst.(*WriteStream)
	if !ok {
		return errdefs.ErrNotImplemented
	}

	r := s.Reader(ctx)
	w := d.Writer(ctx)
	defer w.Close()

	_, err := io.Copy(w, r)
	return err
}
