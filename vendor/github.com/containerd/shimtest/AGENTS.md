# shimtest agent rules

shimtest is a conformance test suite for the containerd shim API. The tests
define how the shim API **must** be used and verify that shim implementations
comply with the API contract.

## Required validation after every change

After **any** change to Go source, `go.mod`, or `go.sum`, run all of the
following and fix every failure before considering the work done:

```sh
# 1. Build
go build ./...

# 2. Linters
golangci-lint run ./...

# 3. Verify formatting
gofmt -l .   # must print nothing
```

## Commit messages

- Subject line must be ≤ 72 characters.
- Every commit must include a `Signed-off-by` trailer (DCO). Use
  `git commit -s` to add it automatically.
- Commits must not have a `From:` line that differs from the `Author:` field.

## Adding new tests

When adding a new test or benchmark:

1. **Update the README table.** Add a row to the appropriate table in
   `README.md` (Tests, Benchmarks, or Planned tests) with the test name,
   feature flag (if any), and a concise description of what the test
   verifies at the API level.

2. **Focus on the shim API contract.** Tests and their comments must describe
   what the shim API requires, not how a particular implementation behaves.
   - Write comments in terms of the API specification: what the caller must
     send, what the shim must return, and what invariants must hold.
   - Do not reference specific shim implementations (e.g., runc, nerdbox) in
     test logic, test names, or comments.
   - Do not add tests that are designed to catch a known bug in a specific
     implementation. If a bug motivates a test, the test should be framed
     as an API conformance requirement, not a regression for that
     implementation.
   - Avoid comments like "this catches the nerdbox close-before-drain race"
     or "runc gets this wrong". Prefer "the shim must deliver all buffered
     output before the wait response is returned".

3. **Keep test names implementation-neutral.** Test names appear in the
   README table and in test output seen by all shim implementors. Names like
   `FastExitOutput` are good; names like `RuncDrainRace` are not.

## Code style

- Follow standard Go conventions (`gofmt`, `goimports`).
- Test helpers shared between suites belong in `helpers.go`.
- Each test suite lives in its own `*_suite.go` file.
- The `testbin` package (`testbin/`) contains the guest-side multicall
  binary; add new guest commands there when a test requires custom
  in-container behaviour.
