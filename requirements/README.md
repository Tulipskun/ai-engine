# Project Requirements

This directory is the source of truth for this repository's product requirements, behavior, constraints, and recorded specification changes.

Requirements belong to the project repository, not to the AI's memory. An implementation task must be checked against the current files here before code is changed.

## Files

- `product.md` — product purpose and goals (single daemon, mobile gateway only).
- `functional.md` — stable functional requirements with IDs.
- `constraints.md` — non-negotiable technical and product constraints.
- `decisions.md` — accepted decisions that affect implementation.
- `changes.md` — requirement changes and their impact.

## Requirement change rule

A new request that conflicts with an existing requirement is a specification change, not a silent implementation detail. The conflict must be identified, the affected requirement updated, the impact recorded in `changes.md`, and architecture/code/tests re-evaluated before implementation is considered complete.

## Project creation rule

When this AI creates a new software project, it must create an equivalent `requirements/` directory in that project's repository and record the initial requirements there before substantial implementation.
