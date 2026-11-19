#!/usr/bin/env python3
"""A small benchmark of whether a lesson learned in one session is used in another.

    run.py run  [--arms a,b] [--cases x,y] [--model M] [--runs N]
    run.py consolidate [--model M] [--dreamer-model M] [--no-probes]
    run.py show TRANSCRIPT.jsonl        print a transcript in readable form
    run.py report RUN_DIR               print the table for a finished run

Each case (cases.json) is a lesson that cannot be known without memory. An arm
is a way of giving Claude Code a memory. For each arm and case:

  teach        a session in repository A does a small task and is corrected
  probe same   a fresh session in repository A meets the same trap
  probe other  a fresh session in repository B meets it

A regular expression grades the file the agent writes. Two things are
recorded: whether the lesson was written to memory after the teach session,
and whether each probe passed. Every call's full transcript is kept.

`consolidate` is a separate test (mess.json): it fills a cmem library with the
kind of mess working agents leave, runs the consolidation routine on it
(routines/consolidate.md), checks the result, probes the raw and the
consolidated library with the same tasks, and finally reverts the run.

Sessions run in containers, one case at a time unless --jobs says otherwise,
and call a model on the owner's subscription; use scripts/bench-transfer.sh, which supplies the token.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

import cmem_arm

HERE = Path(__file__).resolve().parent
# Runs and transcripts go under CARMAN_BENCH_ROOT, which has to be a directory
# the container runtime can mount from. scripts/bench-transfer.sh reads it from
# .local/bench.env.
ROOT = Path(os.environ.get("CARMAN_BENCH_ROOT") or Path.home() / "carman-bench") / "transfer"
IMAGE = os.environ.get("CARMAN_BENCH_IMAGE", "carman-clbench-cmem:2.1.311")
TOKEN = "CLAUDE_CODE_OAUTH_TOKEN"
CONFIG = "/workspace/.claude"

# name -> (teaches, description). What each arm sets up is in prepare().
ARMS = {
    "none": (False, "no memory at all: the floor"),
    "oracle": (False, "built-in memory with the lesson already written: the ceiling"),
    "builtin": (True, "Claude Code's built-in memory, one directory per repository"),
    "builtin_shared": (True, "built-in memory in one directory shared by every repository"),
    "shared_crowded": (True, "the shared directory, already holding a few dozen unrelated lessons"),
    "cmem": (True, "the carman memory, used by hand through the cmem command"),
    "cmem_hook": (True, "the carman memory with the Claude Code hook as installed: index, flashback, notes say who they are for"),
    "cmem_crowded": (True, "cmem by hand, with unrelated lessons added after the lesson is taught"),
    "cmem_hook_crowded": (True, "cmem with the hook, with unrelated lessons added after the lesson is taught"),
    "cmem_hook_wide": (True, "the same, with the index split over five hooks so that it holds every lesson"),
    "cmem_hook_brief": (True, "the same as cmem_hook_crowded, plus a small model that reads what the index left out"),
    "cmem_hook_unscoped": (True, "cmem with the hook as it was before notes had to say who they are for: every note global"),
}
SHARED = ("builtin_shared", "shared_crowded")
CMEM = ("cmem", "cmem_hook", "cmem_crowded", "cmem_hook_crowded", "cmem_hook_wide", "cmem_hook_brief", "cmem_hook_unscoped")
HOOKED = ("cmem_hook", "cmem_hook_crowded", "cmem_hook_wide", "cmem_hook_brief", "cmem_hook_unscoped")

# The directories a case's sessions run in, and the repository each is a
# checkout of. repo-a and checkout-2 are two checkouts of one repository under
# different directory names, as a laptop and a worker would have; repo-b is
# another repository. Each has the git file that names its origin, which is
# all cmem reads to tell repositories apart.
CHECKOUTS = {"repo-a": "acme/alpha", "checkout-2": "acme/alpha", "repo-b": "acme/beta"}
PROBES = {"same": "repo-a", "clone": "checkout-2", "other": "repo-b"}
CMEM_CROWDED = ("cmem_crowded", "cmem_hook_crowded", "cmem_hook_wide", "cmem_hook_brief")

# How many unrelated lessons a crowded arm's memory holds; set by --crowd. Up
# to 22 they come from the retrieval set, beyond that from crowd.json. Past
# about 200 neither memory's index holds them all: Claude Code loads the first
# 200 lines of its index, and cmem's index is cut at 24,000 characters.
CROWD_SIZE = 22

# Which probes follow the teach session; set by --probes.
PROBE_ORDER = ("same", "other")


def crowd() -> list[tuple[str, str]]:
    """Unrelated lessons to fill a memory with: the retrieval set's entries,
    minus the ones the cases themselves teach, then crowd.json's."""
    taught = {"kubectl-context", "topology-spread", "pnpm"}
    entries = json.loads((HERE.parent / "retrieval" / "set.json").read_text())["entries"]
    items = [(e["key"], e["text"]) for e in entries if e["key"] not in taught]
    for index, (_, text) in enumerate(json.loads((HERE / "crowd.json").read_text())):
        words = [w for w in re.findall(r"[a-z0-9]+", text.lower()) if len(w) > 2][:4]
        items.append((f"{'-'.join(words)}-{index}", text))
    return items[:CROWD_SIZE]


def write_memories(memory: Path, items: list[tuple[str, str]]) -> None:
    """Add memories in Claude Code's own format: an index line and a file each."""
    memory.mkdir(parents=True, exist_ok=True)
    with open(memory / "MEMORY.md", "a") as index:
        for key, text in items:
            index.write(f"- [{key.replace('-', ' ')}]({key}.md) — {text.split(';')[0].split('.')[0]}\n")
            (memory / f"{key}.md").write_text(
                f"---\nname: {key}\ndescription: {text.split('.')[0]}\nmetadata:\n  type: feedback\n---\n\n{text}\n"
            )


def slug(path: str) -> str:
    """The name Claude Code gives a project directory under projects/."""
    return re.sub(r"[^A-Za-z0-9]", "-", path)


class Workspace:
    """One arm and case: two repositories and a Claude Code config directory."""

    def __init__(self, arm: str, case: dict, out: Path, model: str, lessons: list[tuple[str, str]]):
        self.arm, self.case, self.out, self.model, self.lessons = arm, case, out, model, lessons
        self.dir = out / "workspace"
        self.env: dict[str, str] = {}
        self.calls: list[dict] = []
        self.service: cmem_arm.Service | None = None
        for repo, origin in CHECKOUTS.items():
            (self.dir / repo / ".git").mkdir(parents=True)
            (self.dir / repo / ".git" / "HEAD").write_text("ref: refs/heads/main\n")
            (self.dir / repo / ".git" / "config").write_text(
                f'[core]\n\trepositoryformatversion = 0\n[remote "origin"]\n\turl = git@git.example.com:{origin}.git\n')
            for name, text in case.get("files", {}).items():
                (self.dir / repo / name).write_text(text)
        (self.dir / ".claude").mkdir()
        self.prepare()

    def memory_dirs(self) -> list[Path]:
        if self.arm in CMEM:
            return []
        if self.arm in SHARED:
            return [self.dir / ".claude" / "shared-memory"]
        return [self.dir / ".claude" / "projects" / slug(f"/workspace/{r}") / "memory" for r in CHECKOUTS]

    def prepare(self) -> None:
        settings: dict = {}
        if self.arm == "none":
            self.env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] = "1"
        if self.arm in SHARED:
            settings["autoMemoryDirectory"] = f"{CONFIG}/shared-memory"
        if self.arm == "oracle" and "lesson" in self.case:
            for memory in self.memory_dirs():
                write_memories(memory, [("lesson", self.case["lesson"])])
        if self.arm == "shared_crowded":
            items = crowd()
            if self.case.get("kind") == "neutral":
                # A neutral task is tried against a memory that also holds the
                # lessons the cases teach, so a near miss can be misapplied.
                items += self.lessons
            write_memories(self.memory_dirs()[0], items)
        if self.arm in CMEM:
            self.service = cmem_arm.Service(self.out / "library")
            self.env.update(self.service.env())
            if self.arm in HOOKED:
                settings.update(cmem_arm.hook_settings(
                    self.service, brief=self.arm == "cmem_hook_brief", parts=5 if self.arm == "cmem_hook_wide" else 1))
                if self.arm == "cmem_hook_brief":
                    (self.dir / ".claude" / "brief.sh").write_text(cmem_arm.BRIEF_SCRIPT)
                if self.arm == "cmem_hook_unscoped":
                    settings["env"].pop("CMEM_NOTE_SCOPE", None)
                    self.env.pop("CMEM_NOTE_SCOPE", None)
            else:
                (self.dir / ".claude" / "CLAUDE.md").write_text(cmem_arm.INSTRUCTION)
        if settings:
            (self.dir / ".claude" / "settings.json").write_text(json.dumps(settings, indent=2) + "\n")

    def close(self) -> None:
        if self.service:
            self.service.stop()

    def memory_text(self) -> str:
        if self.service:
            return self.service.text()
        parts = []
        for memory in self.memory_dirs():
            for path in sorted(memory.rglob("*.md")) if memory.is_dir() else []:
                parts.append(f"## {path.relative_to(self.dir / '.claude')}\n{path.read_text(errors='replace')}")
        return "\n".join(parts)

    def forget_sessions(self) -> None:
        """Remove transcripts so the next session starts fresh; keep memory."""
        projects = self.dir / ".claude" / "projects"
        for project in projects.iterdir() if projects.is_dir() else []:
            for item in project.iterdir():
                if item.name != "memory":
                    shutil.rmtree(item) if item.is_dir() else item.unlink()

    def claude(self, step: str, repo: str, prompt: str, resume: str | None = None) -> dict:
        (self.dir / ".prompt").write_text(prompt)
        args = f'claude -p "$(cat /workspace/.prompt)" --output-format stream-json --verbose ' \
               f"--permission-mode bypassPermissions --model {self.model}"
        if resume:
            args += f" --resume {resume}"
        env = {"CLAUDE_CONFIG_DIR": CONFIG, "IS_SANDBOX": "1", "DISABLE_AUTOUPDATER": "1",
               "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", **self.env}
        cmd = ["docker", "run", "--rm", "-v", f"{self.dir}:/workspace", "-w", f"/workspace/{repo}", "-e", TOKEN]
        child_env = None
        if self.arm == "cmem_hook_brief":
            # The brief starts a model session of its own from inside a hook.
            # In case the harness does not hand its credential down to hooks,
            # the same token is also passed under a second name. By name only,
            # as above: the value is never on a command line or on disk.
            cmd += ["-e", "CARMAN_BRIEF_TOKEN"]
            child_env = {**os.environ, "CARMAN_BRIEF_TOKEN": os.environ[TOKEN]}
        for key, value in env.items():
            cmd += ["-e", f"{key}={value}"]
        cmd += [IMAGE, "sh", "-c", args]
        started = time.time()
        done = subprocess.run(cmd, capture_output=True, text=True, timeout=600, env=child_env)
        transcript = self.out / f"{step}.jsonl"
        transcript.write_text(done.stdout)
        result, session = {}, None
        for line in done.stdout.splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            session = session or event.get("session_id")
            if event.get("type") == "result":
                result = event
        usage = result.get("usage") or {}
        call = {
            "step": step, "session": session, "exit": done.returncode, "seconds": round(time.time() - started, 1),
            "error": bool(result.get("is_error")) or done.returncode != 0 or not result,
            "turns": result.get("num_turns"), "cost": result.get("total_cost_usd") or 0,
            "input": sum(usage.get(k, 0) or 0 for k in ("input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")),
            "output": usage.get("output_tokens", 0) or 0,
            "reply": (result.get("result") or "")[:400],
        }
        if call["error"]:
            call["stderr"] = done.stderr[-400:]
        self.calls.append(call)
        return call

    def grade(self, repo: str, reply: str, leak: bool = False, kept: bool = False) -> tuple[bool, str]:
        path = self.dir / repo / self.case["answer_file"]
        answer = path.read_text(errors="replace") if path.is_file() else ""
        # The file is what was asked for; an answer given only in the reply
        # still shows whether the lesson was applied.
        text = answer or reply
        if leak:
            # A lesson about one repository, probed in another: it passes when
            # the lesson was not applied there.
            ok = not re.search(self.case["leak"], text, re.M)
            return ok, answer.strip()[:300] or f"(no {self.case['answer_file']}; reply: {reply[:200]})"
        if kept:
            # What the correction did not touch has to be there still.
            return bool(re.search(self.case["kept_pass"], text, re.M)), answer.strip()[:300] or f"(no answer file; reply: {reply[:200]})"
        ok = bool(re.search(self.case["pass"], text, re.M))
        if ok and self.case.get("fail") and re.search(self.case["fail"], text, re.M):
            ok = False
        return ok, answer.strip()[:300] or f"(no {self.case['answer_file']}; reply: {reply[:200]})"


def run_case(arm: str, case: dict, out: Path, model: str, lessons: list[tuple[str, str]]) -> dict:
    ws = Workspace(arm, case, out, model, lessons)
    try:
        return run_steps(ws, arm, case, out, model)
    finally:
        ws.close()


def run_document(ws: Workspace, arm: str, case: dict, out: Path, model: str) -> dict:
    """A long entry is already in memory; a session is corrected on one fact
    in it. Afterwards a new session is asked for that fact, and another for a
    fact the correction did not touch."""
    row = {"arm": arm, "case": case["id"], "model": model, "written": None, "same": None, "other": None, "kept": None}
    if not ws.service:
        raise RuntimeError("a document case needs a cmem arm")
    scope = [f"repo:git.example.com/{CHECKOUTS['repo-a']}"]
    seeded = ws.service.call("POST", "/v1/notes", {"kind": "fact", "text": case["document"], "human": "bench", "scope": scope})
    first = ws.claude("update-1-task", "repo-a", case["update"])
    if first["session"] and not first["error"]:
        ws.claude("update-2-correction", "repo-a", case["update_correction"], resume=first["session"])
    memory = ws.memory_text()
    (out / "memory-after-teach.md").write_text(memory)
    row["written"] = bool(re.search(case["written"], memory.replace(case["document"], ""), re.I))
    # How the correction was stored, from what memory holds now.
    entries = ws.service.call("GET", "/v1/entries")
    document = next((e for e in entries if e["id"] == seeded["id"]), None)
    reasons = [f.get("reason", "") for f in (document or {}).get("flags") or []]
    row["stored"] = {
        "document": "kept" if document else "replaced",
        "amended": any(r.startswith("Corrected in part by") for r in reasons),
        "flagged": sum(1 for r in reasons if not r.startswith("Corrected in part by")),
        "current": len(entries),
        "longest": max((e.get("text_length") or len(e.get("text", "")) for e in entries), default=0),
    }
    for name, prompt, kept in (("same", case["probe"], False), ("kept", case["kept_probe"], True)):
        (ws.dir / "repo-a" / case["answer_file"]).unlink(missing_ok=True)
        ws.forget_sessions()
        call = ws.claude(f"probe-{name}", "repo-a", prompt)
        row[name], row[f"{name}_answer"] = ws.grade("repo-a", call["reply"], kept=kept)
    (out / "memory-at-end.md").write_text(ws.memory_text())
    row["scopes"] = [e.get("scope") or [] for e in ws.service.call("GET", "/v1/entries")]
    row["calls"], row["requests"] = ws.calls, ws.service.requests()
    row["errors"] = sum(1 for c in ws.calls if c["error"])
    row["cost"] = round(sum(c["cost"] for c in ws.calls), 4)
    row["input"] = sum(c["input"] for c in ws.calls)
    return row


def run_steps(ws: Workspace, arm: str, case: dict, out: Path, model: str) -> dict:
    if case.get("kind") == "document":
        return run_document(ws, arm, case, out, model)
    neutral = case.get("kind") == "neutral"
    row = {"arm": arm, "case": case["id"], "model": model, "written": None, "same": None, "other": None}
    if ARMS[arm][0] and not neutral:
        first = ws.claude("teach-1-task", "repo-a", case["teach"])
        if first["session"] and not first["error"]:
            ws.claude("teach-2-correction", "repo-a", case["correction"], resume=first["session"])
        if case.get("update"):
            # The lesson changes: a later session is corrected to a new version.
            (out / "memory-after-first-lesson.md").write_text(ws.memory_text())
            (ws.dir / "repo-a" / case["answer_file"]).unlink(missing_ok=True)
            ws.forget_sessions()
            second = ws.claude("update-1-task", "repo-a", case["update"])
            if second["session"] and not second["error"]:
                ws.claude("update-2-correction", "repo-a", case["update_correction"], resume=second["session"])
        if ws.service and arm in CMEM_CROWDED:
            # Added after the lesson, so the crowd is newer than it: the lesson
            # may fall outside the index's budget and has to be found.
            ws.service.seed(crowd())
        memory = ws.memory_text()
        (out / "memory-after-teach.md").write_text(memory)
        row["written"] = bool(re.search(case["written"], memory, re.I))
        answer = ws.dir / "repo-a" / case["answer_file"]
        answer.unlink(missing_ok=True)
        ws.forget_sessions()
    for name in ("same",) if neutral else PROBE_ORDER:
        repo = PROBES[name]
        call = ws.claude(f"probe-{name}", repo, case["probe"])
        row[name], row[f"{name}_answer"] = ws.grade(repo, call["reply"], leak=name == "other" and case.get("kind") == "local")
        ws.forget_sessions()
    (out / "memory-at-end.md").write_text(ws.memory_text())
    if ws.service:
        # Who each entry is shown to: [] is every session everywhere.
        entries = ws.service.call("GET", "/v1/entries")
        row["scopes"] = [e.get("scope") or [] for e in entries]
        if not neutral:
            # A lesson that changed should leave one current entry, unflagged.
            flags = [f.get("reason", "") for e in entries for f in e.get("flags") or []]
            row["stored"] = {"current": len(entries), "amended": any(r.startswith("Corrected in part by") for r in flags),
                             "flagged": sum(1 for r in flags if not r.startswith("Corrected in part by"))}
    row["calls"] = ws.calls
    if ws.service:
        row["requests"] = ws.service.requests()
    row["errors"] = sum(1 for c in ws.calls if c["error"])
    row["cost"] = round(sum(c["cost"] for c in ws.calls), 4)
    row["input"] = sum(c["input"] for c in ws.calls)
    # What the small model behind the brief cost; its sessions are not part of
    # the agent's own and are logged by the brief command.
    briefs = []
    log = ws.dir / ".claude" / "brief.jsonl"
    for line in log.read_text(errors="replace").splitlines() if log.is_file() else []:
        try:
            briefs.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    if briefs:
        calls = [b for b in briefs if "diagnostic" not in b]
        row["briefs"] = len(calls)
        row["brief_cost"] = round(sum(b.get("total_cost_usd") or 0 for b in calls), 4)
        row["brief_replies"] = [(b.get("result") or "")[:200] for b in calls]
        row["brief_diagnostics"] = sorted({b["diagnostic"] for b in briefs if "diagnostic" in b})
        shutil.copy(log, out / "brief.jsonl")
    return row


def consolidate(run_dir: Path, model: str, dreamer_model: str, probes: bool) -> bool:
    """Run the consolidation routine on a messy library and check what it did."""
    mess = json.loads((HERE / "mess.json").read_text())
    retrieval = {e["key"]: e for e in json.loads((HERE.parent / "retrieval" / "set.json").read_text())["entries"]}
    cases = json.loads((HERE / "cases.json").read_text())
    cases = {c["id"]: c for c in cases["cases"] + cases["changing"]}
    routine = (cmem_arm.REPO / "routines" / "consolidate.md").read_text()

    # 1. The mess, as working agents would have left it.
    raw = run_dir / "raw" / "library"
    raw.parent.mkdir(parents=True)
    service = cmem_arm.Service(raw)
    ids: dict[str, str] = {}
    try:
        seeds = [{"key": k, "kind": retrieval[k]["kind"], "text": retrieval[k]["text"], "human": "bench"} for k in mess["crowd"]]
        for item in seeds[:8] + mess["entries"] + seeds[8:]:
            note = {"kind": item["kind"], "text": item["text"], "separate": True, "scope": item.get("scope", [])}
            note.update({k: item[k] for k in ("human", "evidence") if k in item})
            ids[item["key"]] = service.call("POST", "/v1/notes", note)["id"]
            if "flag" in item:
                service.call("POST", f"/v1/entries/{ids[item['key']]}/flag", {"reason": item["flag"]})
    finally:
        service.stop()
    work = run_dir / "consolidated" / "library"
    shutil.copytree(raw, work)

    # 2. The routine, run by a session holding the consolidation credential.
    service = cmem_arm.Service(work)
    try:
        before = service.call("GET", "/v1/entries")
        dreamer = Workspace("none", {"id": "dreamer", "files": {}}, run_dir / "dreamer", dreamer_model, [])
        dreamer.env.update({"CMEM_URL": service.env()["CMEM_URL"], "CMEM_TOKEN": service.dreamer})
        call = dreamer.claude("consolidate", "repo-a", routine)
        after = service.call("GET", "/v1/entries")
        every_run = service.call("GET", "/v1/runs")
    finally:
        service.stop()
    # A consolidator may undo a run of its own and start again; only the runs
    # that stand are counted, and the ones it undid are reported.
    runs = [r for r in every_run if not r.get("reverted_at")]
    undone = [r for r in every_run if r.get("reverted_at")]

    # 3. What it did, checked mechanically. Reading the transcript is still
    # needed: these checks cannot tell whether a merged entry changed meaning.
    current = {e["id"]: e for e in after}
    key_of = {v: k for k, v in ids.items()}
    results = []
    for check in mess["checks"]:
        problems = []
        problems += [f"{k} is still current" for k in check.get("gone", []) if ids[k] in current]
        problems += [f"{k} is no longer current" for k in check.get("kept", []) if ids[k] not in current]
        if check.get("kept_crowd"):
            problems += [f"{k} is no longer current" for k in mess["crowd"] if ids[k] not in current]
        for pattern in check.get("absent", []):
            problems += [f"{e['id']} still says {pattern!r}" for e in after if re.search(pattern, e["text"])]
        # A judgement call: the two entries overlap but differ on a detail, so
        # merging them and leaving both are each defensible. Only loss is wrong.
        problems += [f"no current entry mentions {f!r} any more" for f in check.get("somewhere", [])
                     if not any(re.search(re.escape(f), e["text"], re.I) for e in after)]
        # Scope: an entry holding these words exists with exactly this scope
        # (an empty list means it applies everywhere).
        if "holder" in check:
            holders = [e for e in after if all(re.search(re.escape(f), e["text"], re.I) for f in check["holder"])]
            if not holders:
                problems.append("no current entry holds all of: " + ", ".join(check["holder"]))
            elif not any(sorted(e.get("scope") or []) == sorted(check["scope"]) for e in holders):
                problems.append(f"its scope is {[e.get('scope') for e in holders]}, want {check['scope'] or 'none (global)'}")
            elif check.get("trusted") and not any(e["status"] == "active" for e in holders):
                problems.append("the entry holding it is no longer trusted")
        for pattern in check.get("never_global", []):
            problems += [f"{e['id']} says {pattern!r} and has no scope" for e in after
                         if re.search(pattern, e["text"], re.I) and not e.get("scope")]
        if "facts" in check:
            holders = [e for e in after if all(re.search(re.escape(f), e["text"], re.I) for f in check["facts"])]
            if not holders:
                problems.append("no current entry holds all of: " + ", ".join(check["facts"]))
            elif check.get("trusted") and not any(e["status"] == "active" for e in holders):
                problems.append("the entry holding it is no longer trusted")
        if "count" in check:
            pattern, want = check["count"]
            got = [e["id"] for e in after if re.search(pattern, e["text"])]
            if len(got) != want:
                problems.append(f"{len(got)} current entries match {pattern!r}, want {want}")
        results.append({"check": check["name"], "ok": not problems, "problems": problems})
    size = sum(r["size"] for r in runs)
    summary = {
        "dreamer_model": dreamer_model, "entries_before": len(before), "entries_after": len(after),
        "runs": len(runs), "entries_changed": size, "cap": runs[0]["cap"] if runs else None,
        "runs_undone_by_the_consolidator": [r.get("reverted_because", "") for r in undone],
        "cost": call["cost"], "turns": call["turns"], "error": call["error"], "checks": results,
        "changes": [c | {"keys": [key_of.get(i, i) for i in [c["entry"]] + c.get("replaced", [])]} for r in runs for c in r["changes"]],
    }
    print(f"consolidation by {dreamer_model}: {len(before)} entries before, {len(after)} after; "
          f"{len(runs)} run(s) changing {size} entries; est ${call['cost']:.3f}, {call['turns']} turns"
          + ("; THE SESSION ERRORED" if call["error"] else ""), flush=True)
    for r in undone:
        print(f"  note the consolidator undid a run of its own: {r.get('reverted_because', '')[:160]}", flush=True)
    for r in results:
        print(f"  {'ok  ' if r['ok'] else 'FAIL'} {r['check']}" + "".join(f"\n         {p}" for p in r["problems"]), flush=True)

    # 4. The same probes against the raw and the consolidated memory.
    summary["probes"] = []
    for variant, library in (("raw", raw), ("consolidated", work)) if probes else ():
        for case_id in mess["probes"]:
            case = cases[case_id]
            out = run_dir / f"probe-{variant}" / case_id
            out.mkdir(parents=True)
            shutil.copytree(library, out / "library")
            ws = Workspace("cmem_hook", case, out, model, [])
            try:
                row = {"memory": variant, "case": case_id}
                for name, repo in (("same", "repo-a"), ("other", "repo-b")):
                    reply = ws.claude(f"probe-{name}", repo, case["probe"])
                    row[name], row[f"{name}_answer"] = ws.grade(repo, reply["reply"])
                    ws.forget_sessions()
                row["cost"] = round(sum(c["cost"] for c in ws.calls), 4)
                row["errors"] = sum(1 for c in ws.calls if c["error"])
            finally:
                ws.close()
            summary["probes"].append(row)
            print(f"  probe {variant:<13}{case_id:<17} same={row['same']} other={row['other']} errors={row['errors']}", flush=True)

    # 5. The whole run can be undone: after a revert, memory is as it was.
    if runs:
        service = cmem_arm.Service(work)
        try:
            for r in runs:
                service.call("POST", f"/v1/runs/{r['id']}/revert", {"reason": "Benchmark: checking that a run can be undone."}, dreamer=True)
            restored = {e["id"] for e in service.call("GET", "/v1/entries")}
        finally:
            service.stop()
        summary["revert_restores"] = restored == {e["id"] for e in before}
        print(f"  {'ok  ' if summary['revert_restores'] else 'FAIL'} reverting the run restores memory as it was", flush=True)
    (run_dir / "consolidation.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(f"\ntranscript: {run_dir}/dreamer/consolidate.jsonl   (run.py show FILE)\nlibraries:  {raw}\n            {work}")
    return all(r["ok"] for r in results)


def report(run_dir: Path) -> None:
    rows = [json.loads(line) for line in (run_dir / "results.jsonl").read_text().splitlines()]
    rows.sort(key=lambda r: (r["arm"], r["case"], r.get("run", 0)))
    mark = {True: "pass", False: "FAIL", None: "-"}
    if any("clone" in r for r in rows):
        # With the clone probe: a second checkout of the same repository under
        # another directory name. For a lesson about one repository (kind
        # local), "other repo" passes when the lesson was NOT applied there.
        print(f"{'arm':<19}{'case':<18}{'written':<8}{'same':<6}{'clone':<6}{'other':<6} scopes of what was noted")
        for r in rows:
            written = {True: "yes", False: "NO", None: "-"}[r["written"]]
            scopes = ", ".join("global" if not s else "+".join(s) for s in r.get("scopes", [])) or "-"
            print(f"{r['arm']:<19}{r['case']:<18}{written:<8}{mark[r.get('same')]:<6}{mark[r.get('clone')]:<6}{mark[r.get('other')]:<6} {scopes}")
        print()
    print(f"{'arm':<19}{'case':<18}{'run':>3}  {'written':<8}{'same repo':<10}{'other repo':<11}{'errors':>6} {'est $':>7} {'in ktok':>8}")
    for r in rows:
        written = {True: "yes", False: "NO", None: "-"}[r["written"]]
        print(f"{r['arm']:<19}{r['case']:<18}{r.get('run', 0):>3}  {written:<8}{mark[r.get('same')]:<10}{mark[r.get('other')]:<11}"
              f"{r['errors']:>6} {r['cost']:>7.3f} {r['input'] / 1000:>8.0f}")
    print()
    for arm in dict.fromkeys(r["arm"] for r in rows):
        mine = [r for r in rows if r["arm"] == arm]
        rate = lambda key: f"{sum(1 for r in mine if r.get(key))}/{sum(1 for r in mine if r.get(key) is not None)}"
        tokens = sum(r["input"] for r in mine) / max(sum(len(r["calls"]) for r in mine), 1) / 1000
        brief = sum(r.get("brief_cost", 0) for r in mine)
        clone = f"clone {rate('clone'):<6} " if any("clone" in r for r in mine) else ""
        print(f"{arm:<18} written {rate('written'):<6} same repo {rate('same'):<6} {clone}other repo {rate('other'):<6} "
              f"errors {sum(r['errors'] for r in mine)}  est ${sum(r['cost'] for r in mine):.2f}  {tokens:.1f}k input tokens per call"
              + (f"  briefs {sum(r.get('briefs', 0) for r in mine)} costing ${brief:.3f}" if brief else ""))
    if any(r.get("stored") for r in rows):
        # How each correction ended up in memory.
        print()
        print(f"{'arm':<19}{'case':<18}{'run':>3}  {'changed':<8}{'kept':<6}{'entries':>7}  how the correction was stored")
        for r in rows:
            s = r.get("stored")
            if not s:
                continue
            how = []
            if s.get("document") == "replaced":
                how.append(f"the document was replaced (longest entry now {s.get('longest')} characters)")
            if s.get("amended"):
                how.append("amended: the entry kept, pointing at the note")
            if s.get("flagged"):
                how.append(f"{s['flagged']} plain flag(s)")
            if not how:
                how.append("replaced" if "document" not in s and s["current"] == 1 else "a note beside the old entry, nothing joining them"
                           if s["current"] > 1 else "one entry")
            print(f"{r['arm']:<19}{r['case']:<18}{r.get('run', 0):>3}  {mark[r.get('same')]:<8}{mark[r.get('kept')]:<6}{s['current']:>7}  {'; '.join(how)}")
        for arm in dict.fromkeys(r["arm"] for r in rows):
            mine = [r for r in rows if r["arm"] == arm and r.get("kept") is not None]
            if mine:
                print(f"{arm:<18} changed fact {sum(1 for r in mine if r['same'])}/{len(mine)}  kept fact {sum(1 for r in mine if r['kept'])}/{len(mine)}")
    print(f"\ntranscripts: {run_dir}/<arm>/<case>/run<N>/*.jsonl   (run.py show FILE)")


def show(path: Path) -> None:
    """Print a transcript: what the agent said, the tools it called, what came back."""
    def clip(text: object, n: int = 600) -> str:
        text = text if isinstance(text, str) else json.dumps(text)
        return text if len(text) <= n else text[:n] + f" …[{len(text) - n} more]"
    for line in path.read_text().splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        kind = event.get("type")
        if kind == "system" and event.get("subtype") == "init":
            print(f"[session {event.get('session_id')} model {event.get('model')} cwd {event.get('cwd')}]")
        elif kind in ("assistant", "user"):
            content = (event.get("message") or {}).get("content")
            for block in content if isinstance(content, list) else [{"type": "text", "text": content}]:
                if block.get("type") == "text" and block.get("text"):
                    print(f"{kind.upper()}: {clip(block['text'])}")
                elif block.get("type") == "tool_use":
                    print(f"  TOOL {block.get('name')}: {clip(block.get('input'), 400)}")
                elif block.get("type") == "tool_result":
                    print(f"  RESULT: {clip(block.get('content'), 400)}")
        elif kind == "result":
            print(f"[done: turns {event.get('num_turns')} cost ${event.get('total_cost_usd')} error {event.get('is_error')}]")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    run = sub.add_parser("run")
    run.add_argument("--arms", default="none,oracle")
    run.add_argument("--cases", default="", help="case or task ids; default all of the chosen kind")
    run.add_argument("--kind", default="cases", choices=["cases", "changing", "neutral", "local", "document", "all"])
    run.add_argument("--jobs", type=int, default=1, help="how many arm-and-case runs go on at once; each has its own workspace and service")
    run.add_argument("--probes", default="same,other", help="which of same, clone and other to probe after teaching")
    run.add_argument("--model", default="claude-haiku-5-5")
    run.add_argument("--runs", type=int, default=1)
    run.add_argument("--crowd", type=int, default=22, help="how many unrelated lessons a crowded arm's memory holds (up to 264)")
    tidy = sub.add_parser("consolidate")
    tidy.add_argument("--model", default="claude-sonnet-5-5", help="the model for the probes")
    tidy.add_argument("--dreamer-model", default="claude-sonnet-5-5", help="the model that consolidates")
    tidy.add_argument("--no-probes", action="store_true")
    sub.add_parser("show").add_argument("path", type=Path)
    sub.add_parser("report").add_argument("run_dir", type=Path)
    args = parser.parse_args()

    if args.command == "show":
        show(args.path)
        return 0
    if args.command == "report":
        report(args.run_dir)
        return 0

    if not os.environ.get(TOKEN):
        print(f"{TOKEN} is not set; run this through scripts/bench-transfer.sh", file=sys.stderr)
        return 1
    if args.command == "consolidate":
        run_dir = ROOT / (time.strftime("%Y%m%dT%H%M%S") + "-consolidate")
        run_dir.mkdir(parents=True)
        print(f"run directory: {run_dir}", flush=True)
        consolidate(run_dir, args.model, args.dreamer_model, not args.no_probes)
        return 0
    global CROWD_SIZE, PROBE_ORDER
    CROWD_SIZE = args.crowd
    PROBE_ORDER = tuple(args.probes.split(","))
    if any(p not in PROBES for p in PROBE_ORDER):
        print(f"unknown probe in {args.probes!r}; known: {', '.join(PROBES)}", file=sys.stderr)
        return 1
    data = json.loads((HERE / "cases.json").read_text())
    lessons = [(c["id"], c["lesson"]) for c in data["cases"]]
    cases = {"cases": data["cases"], "neutral": data["neutral"], "changing": data["changing"], "local": data["local"],
             "document": data["document"],
             "all": data["cases"] + data["changing"] + data["local"] + data["neutral"]}[args.kind]
    if args.cases:
        cases = [c for c in cases if c["id"] in args.cases.split(",")]
    arms = args.arms.split(",")
    for arm in arms:
        if arm not in ARMS:
            print(f"unknown arm {arm!r}; known: {', '.join(ARMS)}", file=sys.stderr)
            return 1
    run_dir = ROOT / time.strftime("%Y%m%dT%H%M%S")
    run_dir.mkdir(parents=True)
    print(f"run directory: {run_dir}", flush=True)
    def one(index: int, arm: str, case: dict) -> dict:
        out = run_dir / arm / case["id"] / f"run{index}"
        out.mkdir(parents=True)
        row = run_case(arm, case, out, args.model, lessons)
        row["run"] = index
        return row

    todo = [(index, arm, case) for index in range(args.runs) for arm in arms for case in cases]
    with open(run_dir / "results.jsonl", "a") as results, ThreadPoolExecutor(max_workers=max(args.jobs, 1)) as pool:
        # Rows are written as runs finish, which with --jobs is not the order
        # they were started in; the report sorts them.
        for done in as_completed([pool.submit(one, *item) for item in todo]):
            row = done.result()
            results.write(json.dumps(row) + "\n")
            results.flush()
            print(f"  {row['arm']} {row['case']} run{row['run']}: written={row['written']} same={row['same']} "
                  f"other={row['other']} kept={row.get('kept')} errors={row['errors']}", flush=True)
    report(run_dir)
    return 0


if __name__ == "__main__":
    sys.exit(main())
