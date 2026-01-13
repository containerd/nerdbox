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

type bindMounts []bindMount

type bindMount struct {
	tag    string
	target string
}

func (b *bindMounts) String() string {
	ss := make([]string, 0, len(*b))
	for _, bm := range *b {
		ss = append(ss, bm.tag+":"+bm.target)
	}
	return strings.Join(ss, ",")
}

func (b *bindMounts) Set(value string) error {
	tag, target, ok := strings.Cut(value, ":")
	if !ok || len(tag) == 0 || len(target) == 0 {
		return fmt.Errorf("invalid bind mount %q: expected format: tag:target", value)
	}
	*b = append(*b, bindMount{
		tag:    tag,
		target: target,
	})
	return nil
}

func (b *bindMounts) mountAll(ctx context.Context) error {
	for _, bm := range *b {
		log.G(ctx).WithFields(log.Fields{
			"tag":    bm.tag,
			"target": bm.target,
		}).Info("mounting virtiofs filesystem")

		if err := os.MkdirAll(bm.target, 0700); err != nil {
			return fmt.Errorf("failed to create bind mount target directory %s: %w", bm.target, err)
		}
		if err := mount.All([]mount.Mount{{
			Type:   "virtiofs",
			Source: bm.tag,
			Target: bm.target,
		}}, "/"); err != nil {
			return err
		}
	}
	return nil
}
