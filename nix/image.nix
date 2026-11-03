{
  dockerTools,
  cacert,
  matchstick,
  version,
}:

# A container image with matchstick and its adapter, CA certificates, and nothing else: no shell, a non-root user.
dockerTools.streamLayeredImage {
  name = "matchstick";
  tag = version;
  contents = [
    matchstick
    cacert
  ];
  config = {
    Entrypoint = [ "/bin/matchstick" ];
    Cmd = [
      "run"
      "--config"
      "/etc/matchstick/matchstick.yaml"
    ];
    User = "65532:65532";
    Env = [ "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt" ];
    ExposedPorts."9090/tcp" = { };
  };
}
