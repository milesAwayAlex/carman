# A Claude Code settings file that installs the cmem hook, for passing to
# `claude --settings FILE`. It holds no secret: the token reaches Claude Code
# through its environment, as CMEM_TOKEN.
#
# This mirrors adapters/claude-code/settings.example.json, with the hook
# called by its store path so that it does not depend on PATH. Change one,
# change the other. Options are set with .override, for example
#   cmem-claude-code-settings.override { indexParts = 3; disableAutoMemory = true; }
{
  lib,
  writeText,
  cmem,
  # Where the memory service is reached from this machine.
  url ? "http://127.0.0.1:8377",
  # How many hooks the session index is split over. Claude Code keeps 10,000
  # characters per hook, about 70 lessons; 3 is about what its own memory loads.
  indexParts ? 1,
  # "ask": a new note must say --here or --global. null leaves notes unscoped.
  noteScope ? "ask",
  # Switch Claude Code's own memory off, so that cmem is the only one.
  disableAutoMemory ? false,
  # A sentence added to what a starting session is told when the memory
  # service cannot be reached: what to run on this machine to find out why.
  unreachableHint ? null,
}:

let
  run = arguments: {
    type = "command";
    command = "${lib.getExe cmem} hook ${arguments}claude-code";
    timeout = 5;
  };
  always = [ { hooks = [ (run "") ]; } ];
in
writeText "cmem-claude-code-settings.json" (
  builtins.toJSON (
    {
      env = {
        CMEM_URL = url;
      }
      // lib.optionalAttrs (noteScope != null) { CMEM_NOTE_SCOPE = noteScope; }
      // lib.optionalAttrs (indexParts > 1) { CMEM_INDEX_PARTS = toString indexParts; }
      // lib.optionalAttrs (unreachableHint != null) { CMEM_UNREACHABLE_HINT = unreachableHint; };
      permissions.allow = [
        "Bash(cmem recall *)"
        "Bash(cmem get *)"
        "Bash(cmem note *)"
        "Bash(cmem flag *)"
      ];
      hooks = {
        SessionStart =
          if indexParts > 1 then
            map (part: { hooks = [ (run "--part ${toString part} ") ]; }) (lib.range 1 indexParts)
          else
            always;
        UserPromptSubmit = always;
        PreToolUse = [
          {
            matcher = "Bash";
            hooks = [ (run "") ];
          }
        ];
        PostToolUseFailure = always;
        Stop = always;
      };
    }
    // lib.optionalAttrs disableAutoMemory { autoMemoryEnabled = false; }
  )
)
