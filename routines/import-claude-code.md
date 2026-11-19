# Import Claude Code's built-in memories into cmem

Claude Code keeps its own memories under `~/.claude/projects/`, one directory
per working directory, each holding one markdown file per memory beside a
`MEMORY.md` index. You are moving them into cmem, the memory every session and
every repository shares.

The one judgement to make is which memories apply everywhere. cmem shows an
entry with no scope to every session, and an entry scoped to a repository only
to sessions in it. A wrong "everywhere" is the costly mistake: a rule that was
right in one repository would be followed in all of them.

You do not rewrite anything. `cmem import` stores each file's text as it is,
recorded as told by the owner. You only decide, per file, global or not.

## What the owner gives you

- Their name, for `--human`.
- Which repositories are in active use, and for each the memory directories
  that belong to it, and where a checkout of it is. A repository can have
  several memory directories: one per worktree or subdirectory it was opened
  in. The scope of a repository is what `cmem scope CHECKOUT` prints. It comes
  from the repository's origin, so it is the same for every checkout; do not
  make one up from a directory name.
- Every other memory directory is for a project no longer worked on. Take only
  its global memories.

If any of this is missing, ask before going on.

## Steps

1. For each directory, run `cmem import --dry-run claude-code DIR` to list its
   files, and read each file.
2. Decide which files are global, using the rules below.
3. Show the owner your list before importing anything: each file you would
   make global, with one line on what it says and why it applies everywhere,
   and separately each file you were unsure about. Wait for their answer and
   change the list as they say.
4. Import. For a directory of an active repository:

       cmem import --human NAME --scope "$(cmem scope CHECKOUT)" --global FILE --global FILE claude-code DIR

   For any other directory:

       cmem import --human NAME --globals-only --global FILE claude-code DIR

   A directory with no global file and no active repository is skipped.
5. Report what the commands printed: how many were imported, already there,
   refused and left out.

## What is global

Global:

- who the owner is: their role, what they know, what they are new to;
- how they like to work with an agent: how much to ask, how to report, what to
  do before proposing or building;
- how their machine is set up, where that holds in any repository: which
  context or account a tool points at by default, what is installed.

Not global:

- anything about one codebase: its layout, services, queues, conventions,
  history, or the people on it;
- a permission or rule given for one repository, even when phrased generally.
  "Commit without asking" learned in a personal project is not permission in
  every repository;
- a decision or plan of one project.

When you are unsure, it is not global. A file that mixes the two is not
global; name it in your list so the owner can decide.

## What to expect

- The same preference learned in several projects comes in once: `cmem import`
  skips a file whose text is close to an entry already there, and says so.
  Wording that differs more than that comes in twice; a consolidation pass
  afterwards (`routines/consolidate.md`) merges those.
- A file that looks like it holds a credential is refused. Say which.
- Nothing is removed from Claude Code's directories. Running the import again
  adds nothing.

## Afterwards

Tell the owner that Claude Code's own memory is still switched on, and that
with both on a new lesson may be written to either. Switching it off is
`"autoMemoryEnabled": false` in their Claude Code settings.
