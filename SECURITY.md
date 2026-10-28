# Security Policy

## Supported versions

Security fixes are made on the latest `main` and released in the latest tagged release. Older releases are not patched; upgrade to the latest.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub security advisories: open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a public issue or pull request for a suspected vulnerability.

Include what you found, the version or commit, how to reproduce it, and the impact you expect. You can expect an acknowledgement within a few days. Fixes are developed in a private advisory and published with a release.

## Scope notes

MCP Machine v1 has no authentication by design: anyone who can reach its port can use every server and edit the configuration. It mounts a Podman socket, which is equivalent to root on the host. Deploy it only on a trusted network or behind something that authenticates. Reports about missing authentication on a default install are known limitations (see the README); reports of ways around the documented host and origin guard, or ways to reach the Podman socket beyond what the configuration allows, are in scope.
