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

package sliceutil

func Filter[T any](slice []T, fn func(T) bool) []T {
	var filtered []T
	for _, v := range slice {
		if fn(v) {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

func Map[T any, R any](slice []T, fn func(T) R) []R {
	mapped := make([]R, len(slice))
	for i, v := range slice {
		mapped[i] = fn(v)
	}
	return mapped
}
