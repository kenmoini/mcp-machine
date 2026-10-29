# Security Policy

## Supported versions

Security fixes are made on the latest `main` and released in the latest tagged release. Older releases are not patched; upgrade to the latest.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub security advisories: open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a public issue or pull request for a suspected vulnerability. If private reporting is not enabled on the repository, email the maintainer at the address in `LICENSE`.

<!-- Maintainer: private vulnerability reporting must be enabled in Settings → Code security. -->

Include what you found, the version or commit, how to reproduce it, and the impact you expect. You can expect an acknowledgement within a few days. Fixes are developed in a private advisory and published with a release.

## Scope notes

MCP Machine v1 has no authentication by design: anyone who can reach its port can use every server and edit the configuration. It mounts a Podman socket, which is equivalent to root on the host. Deploy it only on a trusted network or behind something that authenticates. Reports about missing authentication on a default install are known limitations (see the README); reports of ways around the documented host and origin guard, or ways to reach the Podman socket beyond what the configuration allows, are in scope.

## Secrets from Vault

With `${vault:PATH#KEY}` references, a server's secrets are read from Vault
when it starts and delivered into its container through a tmpfs and
`podman exec`. The secret is then never in a configuration file, in the
container's create spec (which Podman persists on disk and shows in `podman
inspect`), in a container label, or in MCP Machine's environment. It is in
the server process's environment, so anything running in that container can
read its own secrets, and so can root on the host; MCP Machine holds it in
memory briefly during delivery and holds its Vault token for its lifetime.
Because the UI has no authentication, anyone who can reach it can define a
server that prints any path MCP Machine's Vault policy allows: scope that
policy to the exact paths the servers use. Reports of a way to get a secret
onto disk, into `podman inspect`, or into another server's container through
the delivery path are in scope.
