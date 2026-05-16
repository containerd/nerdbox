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
$outputDir = Join-Path $PSScriptRoot "..\\_output"
$testBin   = Join-Path $outputDir "integration.test.exe"
if (-not (Test-Path -LiteralPath $testBin)) {
    New-Item -ItemType Directory -Force -Path $outputDir | Out-Null
    & go test -c -o $testBin .
    if ($LASTEXITCODE -ne 0) { exit 1 }
}

# Parse TESTFLAGS forwarded from the task invocation.
#   -run <pattern>  passed to -test.list for test discovery (uses Go's RE2)
#   -v              changes gotestsum output format (handled in Taskfile; skip here)
#   anything else   is normalised to -test.<flag> and forwarded to the binary
$runPattern  = $null
$binaryFlags = @()
if ($env:TESTFLAGS) {
    $tokens = ($env:TESTFLAGS -split '\s+') | Where-Object { $_ }
    for ($i = 0; $i -lt $tokens.Count; $i++) {
        switch -Regex ($tokens[$i]) {
            '^-run$'     { $runPattern = $tokens[++$i]; break }
            '^-run=(.+)' { $runPattern = $Matches[1];  break }
            '^-v$'       { break } # handled by gotestsum format in Taskfile
            '^-test\.'   { $binaryFlags += $tokens[$i]; break }
            '^-(.+)'     {
                $binaryFlags += "-test.$($Matches[1])"
                # If the next token isn't a flag, treat it as the value.
                if (($i + 1) -lt $tokens.Count -and $tokens[$i + 1] -notmatch '^-') {
                    $i++
                    $binaryFlags += $tokens[$i]
                }
                break
            }
        }
    }
}

# Discover tests using -test.list so Go's own RE2 engine applies the -run
# pattern (avoids PowerShell/.NET regex vs RE2 discrepancies).
$listPattern = if ($runPattern) { $runPattern } else { '.*' }
$tests = & $testBin @('-test.list', $listPattern) 2>$null | Where-Object { $_ -match '^Test' }

if (-not $tests) {
    Write-Error "error: no tests found matching '$listPattern'"
    exit 1
}

# Run each test individually. Each test gets its own process so that the WHP
# hypervisor partition is fully released between tests (Windows allows only one
# VM partition per process with the current libkrun build).
$failed = $false
foreach ($test in $tests) {
    $testFlags = @("-test.parallel", "1", "-test.v", "-test.run", $test) + $binaryFlags
    & go tool test2json -t -p "github.com/containerd/nerdbox/integration" $testBin @testFlags
    if ($LASTEXITCODE -ne 0) { $failed = $true }
}

if ($failed) { exit 1 }
