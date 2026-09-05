# Hub TLS material

`docker-compose.yml` bind-mounts this directory read-only at
`/run/nexusgate/tls`. It exists so that mount has a source; the directory is
empty on purpose and nothing here is tracked.

It is only used in `files` mode. The default `NEXUSGATE_TLS_MODE=auto`
generates a self-signed certificate inside the Hub data volume instead, and the
Hub prints its fingerprint on startup — that fingerprint is what a Worker pins,
so `auto` is a complete configuration for a home deployment, not a placeholder
for a real certificate.

For a managed certificate, put the pair here and point the Hub at the container
paths:

```bash
NEXUSGATE_TLS_MODE=files
NEXUSGATE_TLS_CERT_FILE=/run/nexusgate/tls/hub.crt
NEXUSGATE_TLS_KEY_FILE=/run/nexusgate/tls/hub.key
```

Replacing the certificate changes the fingerprint, so every enrolled Worker has
to be re-pinned. `off` is only a controlled HTTP compatibility mode for clients
that do not use browser Sessions; it disables the Hub's own TLS and, with it, the
fingerprint a Worker would pin. Browser administrator Sessions require the Hub to
receive HTTPS directly. An HTTPS termination proxy is not implicitly trusted and
must not be used to make an HTTP Hub appear to support browser Sessions.
