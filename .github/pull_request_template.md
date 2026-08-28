## What this changes

<!-- One or two sentences. What can the project do after this that it could not before? -->

## Why it looks like this

<!-- The reasoning a reviewer cannot get from the diff: what was considered and
     rejected, which SPEC.md decision or constraint this implements. -->

## Verification

<!-- What was actually run, and what it printed. Not "should work". -->

- [ ] `go vet ./...` clean
- [ ] `golangci-lint run` clean
- [ ] `go test ./... -race` green, with `TEST_DATABASE_URL` set so database tests run
- [ ] Exercised by hand where a human can see it (bot reply, HTTP response, log line)

## Spec

- [ ] `SPEC.md` updated in this same commit if a decision or constraint changed
- [ ] New corner cases have tests, not just comments

Relates to: <!-- S-stage from SPEC.md §11, e.g. S2 -->
