---
name: rust-coding
description:
  Rust conventions for this repo. Use when writing, adding or moving Rust code or modules (`*.rs`,
  `Cargo.toml`).
---

# Rust conventions

## Module files

Name a module's file `<name>.rs`, beside a `<name>/` directory that holds its submodules:

```
src/commands.rs          mod cloud;
src/commands/cloud.rs
```

A module with no submodules is a lone `<name>.rs` with no directory. When adding a submodule to it,
create `<name>/` next to the existing file; the file stays where it is.

## Tests

- Strongly prefer only testing exported methods and types.

- Comment each test method with the scenario and expected behavior. Then inside the test body
  comment explaining what data setup is being done, what test methods are being called and what
  scenarios, side effects or post conditions are being verified.
