# 0024. The service runs the command line; the installed binary relays

Status: Proposed (2026-11-19). Built. The owner asked that the memory be
improvable by shipping the service alone, without updating clients.

## Context

`cmem` is one binary, used as the service and as its client (ADR 0012). The
client half held real logic: how a search result is printed, what the session
hook says and when, how the index is laid out, what the help says, which flags
exist. Each improvement to any of these needed a new binary on every machine
that uses the memory. On the owner's machine that means a change to their
machine configuration and a rebuild; the service, by contrast, is one deploy.

The first night of real use produced a dozen such improvements, nearly all of
them in what the client prints.

## Decision

- **The binary sends its command line to the service.** `cmem ARGS` posts the
  arguments, standard input where the command reads it, the caller's `CMEM_*`
  settings, and the scope of the caller's session to `POST /v1/cli`. The
  service runs the command with the same code the binary contains, and
  answers with what to print and the exit code.
- **The caller's token does the work.** Inside the service the command makes
  its requests to the same API, in process, carrying the token the caller
  sent. Nothing can be done through `/v1/cli` that the caller could not do
  through the other routes.
- **What stays in the binary** is what only the caller's machine can do:
  working out the scope of the repository it is in, reading standard input,
  running a brief command, saying at session start that the service cannot be
  reached, and the commands that act on local files (`serve`, `check`,
  `scope`, `import`).
- **A binary meeting an older service** (no `/v1/cli`) runs the command with
  its own copy of the logic. `CMEM_LOCAL=1` asks for that on purpose.
- **The JSON API stays.** Binaries from before this decision keep working
  against a newer service; they just do not get what changed in the logic.

## Consequences

- After one last client update, a change to output, flags, help, the session
  index or flashback behaviour reaches every machine with a service deploy.
- The service and the binary can now differ in version by design. A command
  the binary has never heard of works if the service knows it.
- A second harness needs even less: something that can post a JSON document
  and print the answer (M8).
- `cmem import` and the scope rules remain client logic; changing them still
  needs a client update.
- Every command is now one HTTP round trip to the service plus the in-process
  requests it makes there. For the hook, which runs at every prompt and tool
  call, that is the same single round trip as before.
- The service executes argument parsing on text from callers. It runs only an
  allow-list of commands, none of which opens a path or starts a process, and
  the brief command is never taken from a caller.
