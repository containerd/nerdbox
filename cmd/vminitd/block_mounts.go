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

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
)

type blockMounts []blockMount

type blockMount struct {
	source   string // e.g. /dev/vdc
	target   string // e.g. /mnt/sdc
	readonly bool
}

func (b *blockMounts) String() string {
	ss := make([]string, 0, len(*b))
	for _, bm := range *b {
		s := bm.source + ":" + bm.target
		if bm.readonly {
			s += ":ro"
		}
		ss = append(ss, s)
	}
	return strings.Join(ss, ",")
}

// Set parses a -blockmount flag value of the form: source:target[:ro]
func (b *blockMounts) Set(value string) error {
	source, rest, ok := strings.Cut(value, ":")
	if !ok || len(source) == 0 || len(rest) == 0 {
		return fmt.Errorf("invalid block mount %q: expected format: source:target[:ro]", value)
	}
	target, flags, _ := strings.Cut(rest, ":")
	if len(target) == 0 {
		return fmt.Errorf("invalid block mount %q: target is empty", value)
	}
	readonly := flags == "ro"
	*b = append(*b, blockMount{
		source:   source,
		target:   target,
		readonly: readonly,
	})
	return nil
}

func (b *blockMounts) mountAll(ctx context.Context) error {
	for _, bm := range *b {
		log.G(ctx).WithFields(log.Fields{
			"source":   bm.source,
			"target":   bm.target,
			"readonly": bm.readonly,
		}).Info("mounting ext4 block device")

		if err := os.MkdirAll(bm.target, 0700); err != nil {
			return fmt.Errorf("failed to create block mount target directory %s: %w", bm.target, err)
		}

		var options []string
		if bm.readonly {
			options = []string{"ro"}
		}

		if err := mount.All([]mount.Mount{{
			Type:    "ext4",
			Source:  bm.source,
			Target:  bm.target,
			Options: options,
		}}, "/"); err != nil {
			return fmt.Errorf("failed to mount %s at %s: %w", bm.source, bm.target, err)
		}
	}
	return nil
}
