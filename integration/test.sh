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

# Discover tests from the binary (respects build tags).
readarray -t tests < <(../_output/integration.test -test.list '.*' 2>/dev/null | grep '^Test')

# Parse TESTFLAGS forwarded from the task invocation.
#   -run <pattern>  filters which discovered tests to run (not passed to binary)
#   -v              changes gotestsum output format (handled in Taskfile; skip here)
#   anything else   is normalised to -test.<flag> and forwarded to the binary
run_pattern=""
binary_flags=()
if [ -n "${TESTFLAGS:-}" ]; then
    read -ra tokens <<< "$TESTFLAGS"
    i=0
    while [ $i -lt ${#tokens[@]} ]; do
        tok="${tokens[$i]}"
        case "$tok" in
            -run)      i=$((i+1)); run_pattern="${tokens[$i]}" ;;
            -run=*)    run_pattern="${tok#-run=}" ;;
            -v)        ;; # handled by gotestsum format in Taskfile
            -test.*)   binary_flags+=("$tok") ;;
            -*)        binary_flags+=("-test.${tok#-}") ;;
        esac
        i=$((i+1))
    done
fi
if [ -n "$run_pattern" ]; then
    filtered=()
    for test in "${tests[@]}"; do
        [[ "$test" =~ $run_pattern ]] && filtered+=("$test")
    done
    tests=("${filtered[@]}")
fi

# Run each test individually. Exit code accumulates failures so all tests
# run even when one fails; the final exit code signals gotestsum.
failed=0
for test in "${tests[@]}"; do
    go tool test2json -t -p "github.com/containerd/nerdbox/integration" \
        ../_output/integration.test -test.parallel 1 -test.v -test.run "$test" \
        "${binary_flags[@]}" || failed=1
done
exit $failed
