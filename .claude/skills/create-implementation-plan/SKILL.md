---
name: create-implementation-plan
description: Write the implementation plan for a feature as a sequence of small sub-milestones.
disable-model-invocation: true
argument-hint: <feature description, or a pointer to it, e.g. a roadmap milestone>
---

# Create an implementation plan

Write `docs/plan/<feature>-implementation.md` for the feature in `$ARGUMENTS`, following the layout of the existing
plans there. Read whatever `$ARGUMENTS` points to, then the component docs `DESIGN.md` links for each
component the feature touches.

## Sub-milestones

Split the plan into sub-milestones done in sequence. Each is a small increment: one functionality
added to one component, or one thin slice through several. Each ends with the build green and its
tests passing.

Write each sub-milestone tersely, with:

- **APIs:** each new or changed public API, per component, with its behavior if new or the change
  if changed.
- **Tests:** the cases, each tagged unit, integration or e2e, picked by the rules below.

The last sub-milestone updates `docs/spec`: list each spec doc the feature changes and what changes
in it, so the spec matches the implemented behavior.

## Test levels

- **Integration** is the default: exercise the component against its real dependencies, with
  failures injected through fakes or mocks.
- **Unit** for logic encapsulated in one component, or when the setup cost of real dependencies and
  seed data is too high.
- **E2E** for the happy path only. Cover unhappy paths at the integration or unit level.

When the right level for a case is unclear, list it as a question for the grilling session.

## Grilling

Before writing the plan, put every open decision (test levels you're unsure of, API shapes, scope)
to the user in grilling rounds: numbered questions, each with your recommended answer, asked in one
round per set of questions whose prerequisites are settled. The plan is ready to write when no
question remains open.
