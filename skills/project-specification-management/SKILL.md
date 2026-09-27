# Skill: Project Specification Management

## Purpose

Keep each software project's requirements in that project's repository and make requirement compatibility a mandatory gate before implementation.

## Source of truth

The current repository's `requirements/` directory is authoritative for project intent. Do not replace it with model memory, conversation history, or an unrecorded assumption.

For a project that does not have `requirements/` yet, create it and record the initial requirements before substantial implementation.

## Before implementing a task

1. Read `requirements/product.md`, `requirements/functional.md`, `requirements/constraints.md`, `requirements/decisions.md`, and relevant entries in `requirements/changes.md`.
2. Identify the requirements affected by the request.
3. Check whether the request is compatible with existing requirements and constraints.
4. Check whether the architecture/module boundaries need to change.
5. Only then design and implement code.

## Conflict handling

A conflict is any new request that makes an existing requirement false, weakens a constraint, changes an accepted decision, or changes externally observable behavior that the specification requires.

When a conflict exists:

1. Do not silently implement the conflicting behavior.
2. Identify every affected requirement ID.
3. Propose the exact specification change implied by the user's request.
4. Update the requirement and record the change in `requirements/changes.md` when the request is accepted.
5. Re-check affected architecture, modules, callers, and tests against the new specification.
6. Implement only the resulting specification.

The user's explicit decision to change a requirement takes precedence over the previous requirement, but the change must be recorded in the repository.

## Implementation rules

- Add a function to the module that owns its responsibility.
- Prefer extending an existing module contract when the responsibility already belongs there.
- Create a new module when the responsibility is genuinely new or when an existing module would become a mixed-responsibility unit.
- Do not perform unrelated refactors.
- Do not weaken a requirement merely to make implementation simpler.
- After implementation, verify the changed behavior against the relevant requirement IDs and tests.

## New project initialization

When the AI is asked to create a new software project:

1. Establish the project's goals and non-goals from the user's request.
2. Create `requirements/` in the repository.
3. Write product, functional, constraint, and initial decision requirements.
4. Start implementation only after the initial specification exists in the repository.
5. Update the specification when the project scope changes.

## Completion gate

A task is not complete when code merely works in isolation. It is complete only when:

- the current repository requirements are still satisfied,
- accepted requirement changes are recorded,
- affected module boundaries remain coherent,
- relevant tests or validation have passed, and
- no unrelated behavior was changed without specification.
