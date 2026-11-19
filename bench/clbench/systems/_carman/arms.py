"""The four arms of the carman memory comparison.

Every arm gets the same paragraph about memory, built from one frame, so that
the arms differ in the memory they have and in nothing else. The first pilot
gave each arm its own wording, and the wording alone changed how the agent
worked before any memory existed.
"""

from ...registry import register_system
from .base import CarmanBaseSystem

# What every memory arm is told. Only the description of the mechanism varies.
_FRAME = (
    "What you learn in this session is lost when it ends unless you record it. "
    "{mechanism} "
    "Before you start, look at what earlier sessions recorded. "
    "When feedback arrives, record what a later session should know, and "
    "correct anything recorded that the feedback shows to be wrong."
)

# The no-memory arm gets a paragraph of the same kind and length, so that the
# presence of a paragraph is not itself a difference.
_NONE = (
    "Nothing carries over from one session to the next: there is no memory to "
    "read and nowhere to record anything. Work from what you can find out now, "
    "and use any feedback you are shown on the previous action."
)

# The built-in arm's mechanism sentence. {memory_dir} passes through the frame
# untouched and is filled in later by the benchmark's adapter.
_BUILTIN = (
    "You have a file-based memory at {memory_dir}; MEMORY.md in it is an index "
    "of the other files there. Read and update those files with your file tools."
)

_CMEM = (
    "Your memory is the `cmem` command: `cmem recall WORDS` finds entries, "
    '`cmem note --kind fact|procedure|pitfall|preference --evidence "what you saw" TEXT` '
    "records one, and `cmem flag ID REASON` marks one as wrong."
)

_CMEM_HOOK = _CMEM + " Entries that look relevant are also shown to you without your asking."


@register_system("carman_none")
class CarmanNone(CarmanBaseSystem):
    """No memory."""

    arm = "carman_none"
    auto_memory = False
    default_instruction = _NONE


@register_system("carman_builtin")
class CarmanBuiltin(CarmanBaseSystem):
    """Claude Code's built-in memory directory."""

    arm = "carman_builtin"
    auto_memory = True
    default_instruction = _FRAME.format(mechanism=_BUILTIN)


@register_system("carman_cmem")
class CarmanCmem(CarmanBaseSystem):
    """cmem by hand: recall, note, flag. No hooks."""

    arm = "carman_cmem"
    auto_memory = False
    needs_cmem = True
    default_instruction = _FRAME.format(mechanism=_CMEM)


@register_system("carman_cmem_hook")
class CarmanCmemHook(CarmanBaseSystem):
    """cmem with the Claude Code hooks (session index and flashback)."""

    arm = "carman_cmem_hook"
    auto_memory = False
    needs_cmem = True
    hooks = True
    default_instruction = _FRAME.format(mechanism=_CMEM_HOOK)
