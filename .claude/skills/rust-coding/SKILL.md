---
name: rust-coding
description: Rust conventions for this repo. Use when writing, adding or moving Rust code or modules (`*.rs`, `Cargo.toml`).
---

# Rust conventions

## Module files

Name a module's file `<name>.rs`, beside a `<name>/` directory that holds its submodules:

```
src/commands.rs          mod cws;
src/commands/cws.rs
```

A module with no submodules is a lone `<name>.rs` with no directory. When adding a submodule to it,
create `<name>/` next to the existing file; the file stays where it is.
