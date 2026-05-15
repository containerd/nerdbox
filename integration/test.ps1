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

$ErrorActionPreference = 'Stop'

Set-Location -LiteralPath $PSScriptRoot

# Build the test binary if it doesn't exist
$testBin = Join-Path $PSScriptRoot "..\\_output\\integration.test.exe"
if (-not (Test-Path -LiteralPath $testBin)) {
    & go test -c -o $testBin .
    if ($LASTEXITCODE -ne 0) { exit 1 }
}

# Discover tests from the binary (respects build tags).
# Pass flags as an array to prevent PowerShell splitting on dots.
$tests = & $testBin @('-test.list', '.*') 2>$null | Where-Object { $_ -match '^Test' }

# Run each test individually. Each test gets its own process so that the WHP
# hypervisor partition is fully released between tests (Windows allows only one
# VM partition per process with the current libkrun build).
$failed = $false
foreach ($test in $tests) {
    $testFlags = @("-test.parallel", "1", "-test.v", "-test.run", $test)
    & go tool test2json -t -p "github.com/containerd/nerdbox/integration" $testBin @testFlags
    if ($LASTEXITCODE -ne 0) { $failed = $true }
}

if ($failed) { exit 1 }
