"""One `cmem serve` process on the host, on a free port, with a fresh library."""

from __future__ import annotations

import atexit
import os
import secrets
import signal
import socket
import subprocess
import time
import urllib.request
import weakref
from datetime import datetime, timezone
from pathlib import Path

BENCH_ROOT = Path(os.environ.get("CARMAN_BENCH_ROOT") or Path.home() / "carman-bench")
CARMAN_REPO = Path(os.environ.get("CARMAN_REPO") or Path(__file__).resolve().parents[4])
CMEM_BIN = Path(os.environ.get("CARMAN_CMEM_BIN", str(CARMAN_REPO / "bin" / "cmem")))

# The container reaches the host through this name (Lima).
CONTAINER_HOST = "host.lima.internal"

# Watchdog: if the process that owns the server dies without cleaning up (a
# killed worker), the shell around the server notices and stops it.
_WRAPPER = (
    'cmem_bin="$1"; shift; "$cmem_bin" serve "$@" & sp=$!; '
    'trap \'kill $sp 2>/dev/null\' TERM INT; '
    'while kill -0 "$CARMAN_OWNER_PID" 2>/dev/null && kill -0 $sp 2>/dev/null; '
    "do sleep 1; done; kill $sp 2>/dev/null; wait $sp 2>/dev/null"
)

_live: "weakref.WeakSet[CmemService]" = weakref.WeakSet()


def _stop_all() -> None:
    for svc in list(_live):
        svc.stop()


atexit.register(_stop_all)


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class CmemService:
    """Starts `cmem serve` on a new empty library under BENCH_ROOT/libraries."""

    def __init__(self, label: str):
        if not CMEM_BIN.is_file():
            raise RuntimeError(
                f"host cmem binary not found at {CMEM_BIN}; "
                "build it with: nix develop -c go build -o bin/cmem ./cmd/cmem"
            )
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.library = BENCH_ROOT / "libraries" / f"{label}-{stamp}-{secrets.token_hex(3)}"
        self.library.parent.mkdir(parents=True, exist_ok=True)
        self.log_path = self.library.with_name(self.library.name + ".serve.log")
        self.worker_token = secrets.token_urlsafe(24)
        self._tokens = {
            "laptop": secrets.token_urlsafe(24),
            "worker": self.worker_token,
            "dreamer": secrets.token_urlsafe(24),
        }
        self.port = _free_port()
        self._proc: subprocess.Popen | None = None
        self._start()
        _live.add(self)

    @property
    def host_url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    @property
    def container_url(self) -> str:
        return f"http://{CONTAINER_HOST}:{self.port}"

    def _start(self) -> None:
        env = dict(os.environ)
        env["CMEM_TOKENS"] = ",".join(f"{r}={t}" for r, t in self._tokens.items())
        env["CARMAN_OWNER_PID"] = str(os.getpid())
        log = open(self.log_path, "ab")
        self._proc = subprocess.Popen(
            [
                "sh", "-c", _WRAPPER, "cmem-serve", str(CMEM_BIN),
                "--library", str(self.library),
                "--listen", f"127.0.0.1:{self.port}",
            ],
            env=env,
            stdout=log,
            stderr=log,
            stdin=subprocess.DEVNULL,
            start_new_session=True,
        )
        log.close()
        deadline = time.time() + 20
        while time.time() < deadline:
            if self._proc.poll() is not None:
                raise RuntimeError(
                    f"cmem serve exited early (rc={self._proc.returncode}); see {self.log_path}"
                )
            try:
                with urllib.request.urlopen(self.host_url + "/healthz", timeout=1):
                    return
            except Exception:
                time.sleep(0.1)
        self.stop()
        raise RuntimeError(f"cmem serve did not become healthy; see {self.log_path}")

    def stop(self) -> None:
        proc, self._proc = self._proc, None
        if proc is None:
            return
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            proc.wait()
