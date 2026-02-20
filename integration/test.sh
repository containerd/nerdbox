#!/usr/bin/env bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

set -e

cd "$(dirname "$0")"

# Build the test binary if it doesn't exist
if [[ ! -f ../_output/integration.test ]]; then
    go test -c -o ../_output/integration.test .

    # Sign it with hypervisor entitlement on macOS
    if [[ "$OSTYPE" == "darwin"* ]]; then
        codesign --sign - --entitlements ../cmd/containerd-shim-nerdbox-v1/containerd-shim-nerdbox-v1.entitlements --force ../_output/integration.test &>/dev/null
    fi
fi

# Run each test individually
tests=(
    "TestSystemInfo"
    "TestStreamInitialization"
)

for test in "${tests[@]}"; do
    go tool test2json -t -p "github.com/containerd/nerdbox/integration" ../_output/integration.test -test.parallel 1 -test.v -test.run "$test"
done
