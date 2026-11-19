"""What the transfer benchmark needs to give an agent the carman memory.

A cmem arm gets its own `cmem serve` on the host, on a free port, with a new
empty library. The container reaches it over HTTP. Claude Code's built-in
memory is switched off, so cmem is the only memory the agent has.

How the agent learns that cmem exists differs by arm, as it would in real use:

  cmem       a paragraph in the user-level CLAUDE.md; the agent must think to
             recall and to note
  cmem_hook  the Claude Code hook: the index at session start (which carries
             the same paragraph), and flashback at later moments
"""

from __future__ import annotations

import json
import os
import secrets
import signal
import socket
import subprocess
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
CMEM = Path(os.environ.get("CARMAN_CMEM_BIN", str(REPO / "bin" / "cmem")))
CONTAINER_HOST = "host.lima.internal"  # how a container reaches the host here

# For the by-hand arm. It says what the hook's session-start text says, so the
# two arms differ in delivery and not in what the agent is told.
INSTRUCTION = """# Long-term memory

Your long-term memory is the `cmem` command. Nothing else carries over between sessions.

- Before you start a task, run `cmem recall WORDS` with words that describe it.
- `cmem get ID` shows an entry's sources.
- When you learn something worth keeping, record it with
  `cmem note --kind fact|procedure|pitfall|preference`, then who it is for: `--here` when it is
  true of this repository only (its code, its commands, its own conventions, something you were
  allowed to do in it), or `--global` when it would also hold in another repository (what the
  user prefers, how their machine is set up, how their team or organisation does things
  everywhere); then where it came from, then the text: `--human NAME` when the user told you or
  corrected you (use `user` if you do not know their name), or `--evidence "what you saw"` when
  you found it out yourself.
- If an entry is out of date or wrong and you know what is right, record that in its place:
  add `--replaces ID` to the note. If you only know it is wrong, run `cmem flag ID REASON`.

An entry marked unverified is an agent's own observation that nothing has checked yet: use it,
and check it first when being wrong would be costly.
"""


class Service:
    """One `cmem serve` on a fresh library, stopped with stop()."""

    def __init__(self, library: Path):
        if not CMEM.is_file():
            raise RuntimeError(f"no cmem binary at {CMEM}; build it with: nix develop -c go build -o bin/cmem ./cmd/cmem")
        self.library = library
        # The laptop role: these sessions have a person correcting the agent,
        # and only that role may record that a person said something.
        self.token = secrets.token_urlsafe(24)
        self.dreamer = secrets.token_urlsafe(24)  # the consolidation role
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            self.port = s.getsockname()[1]
        env = {**os.environ, "CMEM_TOKENS": f"laptop={self.token},dreamer={self.dreamer}"}
        log = open(library.with_name(library.name + ".serve.log"), "ab")
        self.process = subprocess.Popen(
            [str(CMEM), "serve", "--library", str(library), "--listen", f"127.0.0.1:{self.port}"],
            env=env, stdout=log, stderr=log, stdin=subprocess.DEVNULL, start_new_session=True,
        )
        log.close()
        deadline = time.time() + 20
        while time.time() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f"cmem serve exited at once; see {log.name}")
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{self.port}/healthz", timeout=1):
                    return
            except OSError:
                time.sleep(0.1)
        self.stop()
        raise RuntimeError(f"cmem serve did not come up; see {log.name}")

    def stop(self) -> None:
        if self.process.poll() is None:
            try:
                os.killpg(self.process.pid, signal.SIGTERM)
                self.process.wait(timeout=5)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                pass

    def env(self) -> dict[str, str]:
        return {
            "CMEM_URL": f"http://{CONTAINER_HOST}:{self.port}",
            "CMEM_TOKEN": self.token,
            "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
            # As the adapter's settings do: a new note must say who it is for.
            # The cmem_hook_unscoped arm takes this back out.
            "CMEM_NOTE_SCOPE": "ask",
        }

    def call(self, method: str, path: str, body: dict | None = None, dreamer: bool = False):
        """One request to the service from the host, as the laptop or the consolidation role."""
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.port}{path}", method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": f"Bearer {self.dreamer if dreamer else self.token}", "Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())

    def seed(self, items: list[tuple[str, str]]) -> None:
        """Add lessons to the library as if a person had told an agent each one."""
        for _, text in items:
            self.call("POST", "/v1/notes", {"kind": "fact", "text": text, "human": "bench", "separate": True})

    def requests(self) -> dict[str, int]:
        """How often each operation was called, from the service's log."""
        counts: dict[str, int] = {}
        log = self.library.with_name(self.library.name + ".serve.log")
        for line in log.read_text(errors="replace").splitlines() if log.is_file() else []:
            if "msg=request" in line and " path=" in line:
                path = line.split(" path=")[1].split()[0]
                name = path.rstrip("/").split("/")[-1] if path.count("/") <= 2 else path.split("/")[2] + "/…/" + path.split("/")[-1]
                counts[name] = counts.get(name, 0) + 1
        return counts

    def text(self) -> str:
        """Everything the agent has stored, as the markdown notes on disk."""
        parts = []
        entries = self.library / "entries"
        for path in sorted(entries.rglob("*.md")) if entries.is_dir() else []:
            parts.append(f"## {path.relative_to(self.library)}\n{path.read_text(errors='replace')}")
        return "\n".join(parts)


# The command the hook runs to have a small model choose from what the index
# left out. The question arrives on standard input. Each call's reply and cost
# are appended to a log the benchmark reads afterwards, with a line saying
# whether a credential was present (never the credential). It exits non-zero
# when the model call failed, so the hook falls back to matching by words.
BRIEF_SCRIPT = """#!/bin/sh
log=/workspace/.claude/brief.jsonl
# Only whether each variable is set is recorded; a value is never expanded
# into anything that is printed.
if [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then inherited=yes; else inherited=no; fi
if [ -n "${CARMAN_BRIEF_TOKEN:-}" ]; then fallback=yes; else fallback=no; fi
echo "{\\"diagnostic\\": \\"credential inherited by the hook: $inherited; passed under a second name: $fallback\\"}" >> "$log"
if [ "$inherited" = no ]; then
    CLAUDE_CODE_OAUTH_TOKEN="$CARMAN_BRIEF_TOKEN"
    export CLAUDE_CODE_OAUTH_TOKEN
fi
reply=$(claude -p --model claude-haiku-5-5 --tools "" --output-format json \\
    --system-prompt "You choose notes from a memory for another agent. Reply with note IDs only.")
echo "$reply" >> "$log"
case "$reply" in *'"is_error":true'*) exit 1 ;; esac
echo "$reply"
"""


def hook_settings(service: Service, brief: bool = False, parts: int = 1) -> dict:
    """Claude Code settings that install the cmem hook, as a user would."""
    settings = json.loads((REPO / "adapters" / "claude-code" / "settings.example.json").read_text())
    env = settings.setdefault("env", {})
    env["CMEM_URL"] = service.env()["CMEM_URL"]
    if parts > 1:
        # One hook's output is capped by Claude Code, so a larger index is
        # delivered as several hooks, one part each.
        env["CMEM_INDEX_PARTS"] = str(parts)
        settings["hooks"]["SessionStart"] = [
            {"hooks": [{"type": "command", "command": f"cmem hook --part {part} claude-code", "timeout": 5}]}
            for part in range(1, parts + 1)
        ]
    if brief:
        env["CMEM_BRIEF_CMD"] = "sh /workspace/.claude/brief.sh"
        # The hook now waits for a model, so it needs longer than five seconds.
        for group in settings["hooks"]["UserPromptSubmit"]:
            for hook in group["hooks"]:
                hook["timeout"] = 90
    return settings
