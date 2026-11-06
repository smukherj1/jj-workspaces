---
name: go-coding
description:
  Go conventions for this repo. Use when writing, adding or modifying Go source files (`*.go`). Do
  not use when writing non-Go files (e.g. markdown docs) that merely contain Go snippets.
---

# Go conventions

## Errors

Wrap every returned error with context at every function layer, even when the callee's error seems
self-explanatory. Describe what the line that failed was trying to do, as a lowercase gerund phrase
without "error" or "failed to". Name the entity involved when the caller's context doesn't already.

```go
if err := foo(); err != nil {
    return fmt.Errorf("starting foo: %w", err)
}

if err := checkMatches(m, o); err != nil {
    return fmt.Errorf("verifying meta matches object %s: %w", o.ID, err)
}
```

`%w` on a gRPC status keeps its code but puts `rpc error: code = ...` in the message clients see.
Build a new status from the original's code and message instead.

## Tests

- Strongly prefer only testing exported methods and types in a Go package.

- Strongly prefer tests with the template Test<ExportedMethod> where <ExportedMethod> is the
  exported type and/or exported method (on the exported type or a standalone method) being tested
  and table driven tests for cases within the method. Test both success and error scenarios if
  possible. A fall back is to use a dedicated Test<ExportedMethod>\_errors for error cases that are
  more complex.

- Use standalone Test<Scenario> only for complex multi-step setup and edge case errors. Avoid these
  unless testing a really critical edge case as these are difficult to follow.

- Comment each test method with the scenario and expected behavior. Then inside the test body
  comment explaining what data setup is being done, what test methods are being called and what
  scenarios, side effects or post conditions are being verified.
