"""Base class for the carman benchmark arms.

A Claude Code system that (1) keeps workspaces where the Lima VM can see them,
(2) authenticates with the subscription token passed by name, (3) uses a
prebuilt image, (4) scrubs everything except the arm's memory between
instances, and (5) records token usage per Claude Code session.
"""

from __future__ import annotations

import atexit
import json
import logging
import os
import stat
import subprocess
import tempfile
import threading
import weakref
from pathlib import Path
from typing import Any, Optional

from ...interface import Query, Response, Observation, observation_marks_instance_complete
from ..claude import system as claude_system
from ..claude.system import ClaudeCodeSystem
from ..common import CONTAINER_WORKSPACE, stop_docker_container
from .cmem_service import BENCH_ROOT, CARMAN_REPO, CmemService

logger = logging.getLogger(__name__)

CLAUDE_CODE_VERSION = "2.1.311"
DEFAULT_MODEL = "claude-haiku-5-5"
IMAGE_CLAUDE = f"carman-clbench-claude:{CLAUDE_CODE_VERSION}"
IMAGE_CMEM = f"carman-clbench-cmem:{CLAUDE_CODE_VERSION}"
TOKEN_VAR = "CLAUDE_CODE_OAUTH_TOKEN"
DEFAULT_TOKEN_FILE = str(CARMAN_REPO / ".local" / "bench-token")
WORKSPACES = BENCH_ROOT / "workspaces"
USAGE_DIR = BENCH_ROOT / "usage"
_CONFIG = f"{CONTAINER_WORKSPACE}/.claude"


def resolve_oauth_token() -> str:
    """Return the subscription token: from the environment, else from a 0600 file.

    Never logs or prints the token. Raises with a clear message otherwise.
    """
    if os.environ.get("ANTHROPIC_API_KEY"):
        raise RuntimeError(
            "ANTHROPIC_API_KEY is set. The carman arms use the subscription token "
            f"({TOKEN_VAR}) only; unset ANTHROPIC_API_KEY and run again."
        )
    token = os.environ.get(TOKEN_VAR, "").strip()
    if token:
        return token
    path = Path(os.environ.get("CARMAN_BENCH_TOKEN_FILE") or DEFAULT_TOKEN_FILE)
    if not path.is_file():
        raise RuntimeError(
            f"{TOKEN_VAR} is not set and the token file {path} does not exist. "
            "Create the token with `claude setup-token`, put it in that file "
            "with mode 600, or export it."
        )
    if stat.S_IMODE(path.stat().st_mode) & 0o077:
        raise RuntimeError(
            f"token file {path} is readable by group or others; run `chmod 600 {path}`."
        )
    token = path.read_text().strip()
    if not token:
        raise RuntimeError(f"token file {path} is empty.")
    return token


# Per-thread workspace root for the stock adapter's workspace creation.
_tls = threading.local()
_orig_create_run_workspace = claude_system.create_run_workspace


def _create_run_workspace(cache_dir_name: str) -> str:
    root = getattr(_tls, "workspace_root", None)
    if root is None:
        return _orig_create_run_workspace(cache_dir_name)
    Path(root).mkdir(parents=True, exist_ok=True)
    return tempfile.mkdtemp(prefix="run_", dir=str(root))


if getattr(claude_system.create_run_workspace, "_carman", None) is None:
    _create_run_workspace._carman = True  # type: ignore[attr-defined]
    claude_system.create_run_workspace = _create_run_workspace

_live: "weakref.WeakSet[CarmanBaseSystem]" = weakref.WeakSet()


def _cleanup_all() -> None:
    for system in list(_live):
        system._shutdown()


atexit.register(_cleanup_all)


class CarmanBaseSystem(ClaudeCodeSystem):
    """Claude Code on a subscription, fresh session per instance, arm-scoped memory."""

    arm = "base"
    # The arms are compared with each other; carman_none is the baseline.
    supports_baseline = False
    needs_cmem = False
    hooks = False
    auto_memory = True
    default_instruction = ""

    def __init__(
        self,
        model: str = DEFAULT_MODEL,
        name: Optional[str] = None,
        single_conversation: bool = False,
        timeout: int = 300,
        max_turns: Optional[int] = None,
        allowed_tools: Optional[list[str]] = None,
        disallowed_tools: Optional[list[str]] = None,
        docker_image: Optional[str] = None,
        version: str = CLAUDE_CODE_VERSION,
        reasoning_effort: Optional[str] = "low",
        memory_instruction: Optional[str] = None,
    ):
        self._oauth_token = resolve_oauth_token()
        self._cmem: CmemService | None = None
        self._pristine = True
        self._epoch = 0
        self._current_query: Query | None = None
        self._session_usage: list[dict[str, Any]] = []
        self._turn_usage: list[dict[str, Any]] = []
        self._image = docker_image or (IMAGE_CMEM if self.needs_cmem else IMAGE_CLAUDE)
        if memory_instruction is None:
            memory_instruction = self.default_instruction

        if self.needs_cmem:
            self._cmem = CmemService(self.arm)
        _tls.workspace_root = str(WORKSPACES)
        try:
            super().__init__(
                model=model,
                name=name or self.arm,
                timeout=timeout,
                max_turns=max_turns,
                allowed_tools=allowed_tools,
                disallowed_tools=disallowed_tools,
                docker_image=self._image,
                version=version,
                memory_instruction=memory_instruction,
                single_conversation=single_conversation,
                api_key="subscription-oauth-unused",
                reasoning_effort=reasoning_effort,
            )
        except BaseException:
            self._shutdown()
            raise
        finally:
            _tls.workspace_root = None
        _live.add(self)

    # ---- container -------------------------------------------------------

    def _container_env(self) -> dict[str, str]:
        env = {
            "CLAUDE_CONFIG_DIR": _CONFIG,
            "IS_SANDBOX": "1",
            "DISABLE_AUTOUPDATER": "1",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        }
        if not self.auto_memory:
            env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] = "1"
        if self._cmem is not None:
            env["CMEM_URL"] = self._cmem.container_url
            if self.hooks:
                env["CMEM_SCOPE"] = "bench"
        return env

    def _secret_env(self) -> dict[str, str]:
        secrets = {TOKEN_VAR: self._oauth_token}
        if self._cmem is not None:
            secrets["CMEM_TOKEN"] = self._cmem.worker_token
        return secrets

    def _start_container(self) -> None:
        """Start the container from the prebuilt image. Secrets go in by name."""
        self._epoch += 1
        name = f"carman_bench_{Path(self._tmp_dir).name}_{self._epoch}"
        cmd = ["docker", "run", "-d", "--name", name, "-v", f"{self._tmp_dir}:{CONTAINER_WORKSPACE}"]
        for key, value in self._container_env().items():
            cmd += ["-e", f"{key}={value}"]
        secrets = self._secret_env()
        for key in secrets:
            cmd += ["-e", key]  # value comes from the subprocess environment
        cmd += ["-w", CONTAINER_WORKSPACE, self._docker_image, "sleep", "infinity"]
        run_env = {**os.environ, **secrets}
        result = subprocess.run(cmd, capture_output=True, text=True, check=False, env=run_env)
        if result.returncode != 0:
            raise RuntimeError(f"Failed to start container: {result.stderr.strip()}")
        self._container_id = result.stdout.strip()
        verify = self._docker_exec("claude --version", timeout=30)
        out = (verify.stdout or verify.stderr).strip()
        if verify.returncode != 0 or self._version not in out:
            raise RuntimeError(f"unexpected Claude Code in image {self._docker_image}: {out!r}")
        logger.info("Container %s ready: %s", name, out)

    # ---- workspace -------------------------------------------------------

    def _initialize_workspace(self) -> None:
        super()._initialize_workspace()
        if self.hooks:
            self._write_hook_settings()

    def _write_hook_settings(self) -> None:
        example = CARMAN_REPO / "adapters" / "claude-code" / "settings.example.json"
        settings = json.loads(example.read_text())
        assert self._cmem is not None
        settings.setdefault("env", {})["CMEM_URL"] = self._cmem.container_url
        path = Path(self._tmp_dir) / ".claude" / "settings.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(settings, indent=2) + "\n")

    def _scrub(self, keep_memory: bool) -> None:
        """Remove everything an agent could carry between sessions except memory.

        In /workspace: everything except .claude. In .claude: everything except
        settings.json, .claude.json and projects. In projects: transcripts and
        per-session state, keeping each project's memory directory only when
        keep_memory (the built-in memory arm). In the container outside
        /workspace: /tmp and the home directory's Claude and cache state.
        """
        projects = (
            "find projects -mindepth 2 -maxdepth 2 ! -name memory -exec rm -rf {} +"
            if keep_memory
            else "rm -rf projects"
        )
        script = (
            "cd /workspace && "
            "find . -mindepth 1 -maxdepth 1 ! -name .claude -exec rm -rf {} + ; "
            "if [ -d .claude ]; then cd .claude && "
            "find . -mindepth 1 -maxdepth 1 ! -name settings.json ! -name .claude.json "
            "! -name projects -exec rm -rf {} + ; "
            f"[ -d projects ] && {projects} ; fi ; "
            "rm -rf /tmp/* /root/.claude /root/.claude.json /root/.cache /root/.npm ; true"
        )
        res = self._docker_exec(script, timeout=60)
        if res.returncode != 0:
            raise RuntimeError(f"workspace scrub failed: {res.stderr.strip()}")

    # ---- instance boundaries and reset -----------------------------------

    def observe(self, observation: Observation, next_query: Optional[Query] = None) -> None:
        super().observe(observation, next_query)
        if not self._single_conversation and observation_marks_instance_complete(observation):
            self._scrub(keep_memory=self.auto_memory)

    def _clear_persistent_workspace_state(self) -> None:
        # A reset starts a new memory, so the memory directory goes too.
        self._scrub(keep_memory=False)
        super()._clear_persistent_workspace_state()

    def reset(self) -> None:
        if not self._pristine:
            self._stop_container()
            if self.needs_cmem:
                if self._cmem is not None:
                    self._cmem.stop()
                self._cmem = CmemService(self.arm)
            self._start_container()
        self._session_usage = []
        super().reset()
        self._pristine = True

    # ---- usage -----------------------------------------------------------

    @property
    def usage_log_path(self) -> Path:
        return USAGE_DIR / f"{self.arm}-{Path(self._tmp_dir).name}.jsonl"

    def respond(self, query: Query) -> Response:
        self._current_query = query
        self._pristine = False
        self._turn_usage = []
        response = super().respond(query)
        if isinstance(response.metadata, dict):
            response.metadata["carman_arm"] = self.arm
            response.metadata["carman_session_usage"] = list(self._turn_usage)
        return response

    def _run_claude(self, prompt: str, resume_mode: str) -> list[dict[str, Any]]:
        events = super()._run_claude(prompt, resume_mode)
        self._record_session_usage(events)
        return events

    def _record_session_usage(self, events: list[dict[str, Any]]) -> None:
        q = self._current_query
        for event in events:
            if event.get("type") != "result":
                continue
            usage = event.get("usage") or {}
            record = {
                "arm": self.arm,
                "epoch": self._epoch,
                "instance_id": getattr(q, "instance_id", None),
                "instance_index": getattr(q, "instance_index", None),
                "call": len(self._session_usage) + 1,
                "session_id": event.get("session_id"),
                "input_tokens": int(usage.get("input_tokens", 0) or 0),
                "cache_read_input_tokens": int(usage.get("cache_read_input_tokens", 0) or 0),
                "cache_creation_input_tokens": int(usage.get("cache_creation_input_tokens", 0) or 0),
                "output_tokens": int(usage.get("output_tokens", 0) or 0),
                "total_cost_usd": event.get("total_cost_usd"),
                "num_turns": event.get("num_turns"),
                "duration_ms": event.get("duration_ms"),
                "is_error": event.get("is_error"),
            }
            self._session_usage.append(record)
            self._turn_usage.append(record)
            try:
                USAGE_DIR.mkdir(parents=True, exist_ok=True)
                with open(self.usage_log_path, "a") as fh:
                    fh.write(json.dumps(record) + "\n")
            except OSError as exc:
                logger.warning("could not write usage log: %s", exc)

    def get_run_artifacts(self) -> dict[str, Any]:
        artifacts = super().get_run_artifacts()
        artifacts["carman_arm"] = self.arm
        artifacts["carman_session_usage"] = list(self._session_usage)
        artifacts["carman_usage_log"] = str(self.usage_log_path)
        artifacts["carman_library"] = str(self._cmem.library) if self._cmem else None
        return artifacts

    # ---- shutdown --------------------------------------------------------

    def _shutdown(self) -> None:
        try:
            self._stop_container()
        except Exception:
            pass
        cmem = getattr(self, "_cmem", None)
        if cmem is not None:
            cmem.stop()

    def __del__(self) -> None:
        try:
            super().__del__()
        finally:
            self._shutdown()
