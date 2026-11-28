# A Claude Code settings file that installs the dispatcher's messaging hooks
# for sessions on a person's own machine (ADR 0049), for passing to
# `claude --settings FILE` or merging with another. It holds no secret: the
# token reaches Claude Code through its environment, as DISPATCHER_TOKEN.
#
# This mirrors adapters/claude-code/dispatcher.settings.example.json, with
# the hooks called by their store path so that they do not depend on PATH and
# stay at the same pin as the binary. Change one, change the other. Options
# are set with .override, for example
#   dispatcher-claude-code-settings.override { place = "laptop"; allow = false; }
{
  lib,
  writeText,
  dispatcher,
  # Where the dispatcher is reached from this machine.
  url ? "http://127.0.0.1:8379",
  # The name of this machine's place, as the installation's values have it.
  place ? "host",
  # Let sessions run the messaging commands without asking. Sending to an
  # agent by name starts a run of it, so this is the machine owner's call.
  allow ? true,
}:

let
  exe = lib.getExe dispatcher;
  now = {
    type = "command";
    command = "${exe} hook claude-code";
    timeout = 10;
  };
  # The idle listener: it waits for a message and wakes the session.
  listen = {
    type = "command";
    command = "${exe} hook claude-code --wait 335h";
    asyncRewake = true;
    # What the woken session and its terminal are shown before the message,
    # in place of the harness's "Stop hook blocking error" (seen in 2.1.323 and 2.1.326).
    rewakeMessage = "A message for this session arrived through the dispatcher (it is not an error):";
    rewakeSummary = "A message arrived through the dispatcher";
    timeout = 1209600;
  };
  always = [ { hooks = [ now ]; } ];
  andListen = [
    {
      hooks = [
        now
        listen
      ];
    }
  ];
in
writeText "dispatcher-claude-code-settings.json" (
  builtins.toJSON (
    {
      env = {
        DISPATCHER_URL = url;
        DISPATCHER_PLACE = place;
      };
      hooks = {
        SessionStart = andListen;
        UserPromptSubmit = always;
        PostToolBatch = always;
        Stop = andListen;
        SessionEnd = always;
      };
    }
    // lib.optionalAttrs allow {
      permissions.allow = map (c: "Bash(${c})") [
        "dispatcher peers*"
        "dispatcher send *"
        "dispatcher reply *"
        "dispatcher inbox*"
        "dispatcher read *"
        "dispatcher message *"
        "dispatcher watch *"
        "dispatcher timed*"
        "dispatcher whoami*"
      ];
    }
  )
)
