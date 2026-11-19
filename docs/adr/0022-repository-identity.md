# 0022. A repository is named by its origin remote, not by its directory

Status: Accepted (2026-11-19). Built. The owner asked that scopes not depend
on directory names, which differ between environments.

## Context

A scope tag such as `repo:carman` decides which sessions see an entry. The
hook worked the tag out from the name of the directory the repository was
checked out in. The same repository has different directory names in a linked
worktree, in a second clone, and in a worker's fresh checkout, so an entry
scoped on the laptop would not have been seen by a worker, or the reverse.

## Decision

- A repository's scope is `repo:` followed by its `origin` remote in a
  canonical form: host and path, lower-cased, with no scheme, user, password,
  port or `.git`. The ssh and https forms of one remote give the same tag:
  `repo:github.com/acme/billing`.
- With no origin, a repository's only other remote is used. With no usable
  remote, or one that is a path on the machine, the tag falls back to the
  directory name of the main checkout, which is at least the same for all of
  the repository's worktrees.
- `CMEM_SCOPE`, when a harness sets it, still wins.
- The git files are read directly (`.git/config`, and for a worktree the files
  that point at the shared git data). Nothing runs git: the hook is called
  many times a session, and it must work where git is missing or refuses a
  directory it does not own. In the benchmark's containers git was never
  installed, so the old code had been falling back to the directory name
  there all along.
- `cmem scope [DIR]` prints the tag, for where one has to be typed, such as
  `cmem import --scope`. `cmem note --here` uses it for the directory the
  agent is in.

## Consequences

- A fork has a different origin from the repository it was forked from, and so
  a different scope. One person's clones all share an origin, which is the
  case that matters here.
- Renaming or moving a repository on its host changes its scope, and entries
  under the old tag stop being shown there. A consolidation pass can move
  them, by the owner's instruction; nothing does it automatically.
- git's URL rewriting (`insteadOf`) and included config files are not read.
- The tag carries the host and the owner's or organisation's name. It is data
  in the owner's library, not in this repository.
