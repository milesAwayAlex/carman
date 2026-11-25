# dispatcher: the carman dispatcher, as one binary. `dispatcher serve` is the
# service and the other commands are its client.
{
  lib,
  buildGoModule,
  cmem,
}:

buildGoModule {
  pname = "dispatcher";
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

  subPackages = [ "cmd/dispatcher" ];

  # One module, one go.sum: the same hash as cmem's (nix/packages/cmem.nix
  # says how to set it when go.sum changes).
  inherit (cmem) vendorHash;

  # The tests need Postgres; scripts/test.sh runs them.
  doCheck = false;

  meta = {
    description = "The carman dispatcher: starts agents' runs as Kubernetes Jobs and records how every Job ended";
    license = lib.licenses.mit;
    mainProgram = "dispatcher";
  };
}
