{
  lib,
  buildGo127Module,
  version,
}:

buildGo127Module {
  pname = "matchstick";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
      ../examples
    ];
  };

  vendorHash = "sha256-SiFud0jShOva+fumPw7Ly4VxZ0QXcew/hqDSfP1iPrk=";

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];
  # -short skips the adapter's test that builds matchstick with the go command.
  checkFlags = [ "-short" ];

  meta = {
    description = "Ephemeral GitHub Actions runners: one fresh cloud VM per job";
    homepage = "https://github.com/fakta-ai/matchstick";
    license = lib.licenses.mit;
    mainProgram = "matchstick";
  };
}
