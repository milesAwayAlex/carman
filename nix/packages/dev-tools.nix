# What working on this repository needs besides a shell and git: the tools
# its checks and its deploy script call. The development shell has them, and
# as a package they can be put where there is no Nix to run a shell with,
# such as an agent's image.
{
  buildEnv,
  go,
  kubernetes-helm,
}:

buildEnv {
  name = "carman-dev-tools";
  paths = [
    go
    kubernetes-helm
  ];
}
