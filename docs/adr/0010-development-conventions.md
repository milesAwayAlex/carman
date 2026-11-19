# 0010. Development conventions for the memory service

Status: Proposed (2026-11-18). Revised the same day when storage moved from
Postgres to markdown notes (0015).

## Context

The first code needed several choices that no document covered: how to get a
toolchain, how to run the requirement checks, and what to call the module. The
owner's machine uses Nix and had no Go installed.

## Decision

- **Toolchain.** A Nix flake (`flake.nix`) provides Go and git. `flake.lock`
  pins the versions. Everything runs through `nix develop`.
- **Rules where they cannot be bypassed.** A requirement is enforced at the
  lowest layer that can enforce it (the store, not only the HTTP handlers).
- **Tests.** Each test runs against a real library in a temporary directory,
  with real git. Tests that check a requirement carry its ID in their name.
  Nothing is skipped when a dependency is missing; it fails.
- **Checks.** `scripts/test.sh` runs the format check, vet and the tests.
- **Module path.** `carman`, a placeholder until the repository has a home.

## Consequences

- Anyone without Nix needs Go and git on their PATH to run the scripts.
- The module path will need a find-and-replace once the repository has a
  remote.
