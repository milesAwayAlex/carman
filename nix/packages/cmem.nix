# cmem: the carman memory, as one binary. `cmem serve` is the service and the
# other commands are its client, including the Claude Code hook.
{
  lib,
  buildGoModule,
  makeWrapper,
  git,
}:

buildGoModule {
  pname = "cmem";
  version = "0.1.0";

  # Only what the binary is built from, so that editing documents, benchmarks
  # or deployment files does not rebuild it.
  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ../../go.mod
      ../../go.sum
      ../../cmd
      ../../internal
    ];
  };

  subPackages = [ "cmd/cmem" ];

  # The hash of the Go modules named in go.sum. When go.sum changes, set this
  # to lib.fakeHash, build, and copy the hash the failure reports.
  vendorHash = "sha256-g4hTXJSV6jWq/3bVTCcTfyKwn0K33zlXltH/ADdflCI=";

  # The tests start git and local HTTP servers and take a minute; they are
  # run by scripts/test.sh rather than on every build.
  doCheck = false;

  nativeBuildInputs = [ makeWrapper ];

  # The service keeps its library in git, and the hook asks git which
  # repository a session is in. A git already on PATH is preferred.
  postInstall = ''
    wrapProgram $out/bin/cmem --suffix PATH : ${lib.makeBinPath [ git ]}
  '';

  meta = {
    description = "The carman memory: a long-term memory service for agents, and its client";
    license = lib.licenses.mit;
    mainProgram = "cmem";
  };
}
