# Repository Instructions

## Specification

For all coding work in this repository, read `index.md` first, then `requirements/README.md` and the relevant files under `requirements/` before changing code.

After any code change, update `index.md` when structure or key files changed and update `requirements/` plus `requirements/changes.md` when behavior or spec changed.

Treat repository requirements as the source of truth for project behavior. Do not substitute model memory or chat history for the repository specification.

When a new task conflicts with an existing requirement, handle it as a specification change: identify the conflict, update the requirement after the user's decision, record the change in `requirements/changes.md`, then re-evaluate architecture, modules, callers, and tests.

When creating another software project, create that project's own `requirements/` directory in its repository and write the initial specification before substantial implementation.

## Architecture and implementation

Keep responsibilities modular. Put new functionality in the module that owns the responsibility. Create a new module when the responsibility is genuinely new or the existing module would become mixed-responsibility.

Avoid unrelated refactors. Verify changed behavior against the applicable requirement IDs and tests before declaring the task complete.

The detailed procedure is defined in `skills/project-specification-management/SKILL.md`.
