# carman: the owner's way to the sessions of an installation. `carman go`
# lists the sessions the dispatcher knows and puts the terminal in one
# (ADR 0057). It runs kubectl, and fzf for its picker when there is one;
# both are taken from PATH, since which kubectl reaches the cluster is the
# machine's business.
# `carman status`, `plan` and `queue` are the dispatcher's client's own
# views: it is built in by its path, so that nothing has to be on PATH.
{
  lib,
  buildGoModule,
  cmem,
  dispatcher,
}:

buildGoModule {
  pname = "carman";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ../../go.mod
      ../../go.sum
      ../../cmd
      ../../internal
    ];
  };

  subPackages = [ "cmd/carman" ];

  ldflags = [ "-X main.dispatcherBin=${lib.getExe dispatcher}" ];

  # One module, one go.sum: the same hash as cmem's.
  inherit (cmem) vendorHash;

  meta = {
    description = "The owner's way to an installation's sessions: carman go lists them and goes to one";
    license = lib.licenses.mit;
    mainProgram = "carman";
  };
}
