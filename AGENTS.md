# JJ Workspaces Project

See dir `docs` for documentation:

- `docs/spec` for the specification of the project:
  - [PRD](docs/spec/PRD.md) and technical [design](docs/spec/DESIGN.md). Always read these to
    understand the project. Read the component documents the design links to only if you're
    changing or debugging that component and need its detailed APIs and behavior, including the
    APIs of components it calls or is called by.
- `docs/plan` for planned or in-progress changes in the project. Always update the spec after
  implementing changes in a plan. Update the plan (marking sections or it as done) after implementing
  part of the plan. Only read the plan document you're asked to execute on. Also read any other
  documents the plan document you're implementing references.
  - [MVP roadmap](docs/plan/mvp-impl-roadmap.md) is the source of truth for milestones and open
    questions. List each open question under the milestone it blocks, not in the spec.
