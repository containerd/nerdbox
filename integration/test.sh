#!/bin/bash
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
