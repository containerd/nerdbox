//go:build !linux

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

// pseudoFS reports the name of the kernel-generated filesystem holding
// name, or "" if name is on an ordinary filesystem.
//
// Container filesystems are only ever resolved inside the Linux guest, so
// elsewhere every path is treated as ordinary and the archive operations
// behave as they did before the check existed.
func pseudoFS(string) string { return "" }

// pseudoFSHolder reports the kernel-generated filesystem that dir would be
// created on, or "" if that filesystem is an ordinary one.
func pseudoFSHolder(string) string { return "" }
