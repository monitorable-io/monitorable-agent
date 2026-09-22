# Security policy

The agent runs on customers' servers as an unprivileged, sandboxed systemd service. The
security model — what the unit may touch, how SMART access is tiered, what `SHA256SUMS`
does and does not protect — is described in [README.md](README.md#security-model) and
[distribution/README.md](distribution/README.md#integrity).

## Reporting a vulnerability

Please do **not** open a public issue for a security problem.

- Preferred: [report privately through GitHub](https://github.com/monitorable-io/monitorable-agent/security/advisories/new)
  (private vulnerability reporting is enabled on this repository).
- Alternatively, email hello@monitorable.io with `security` in the subject.

Include the agent version (`/opt/monitorable/monitorable-agent --version`, or the
`agentVersion` shown in the dashboard), the host OS, and steps to reproduce. You will get
an acknowledgement within a few days and a fix or a mitigation before any public
disclosure.

## Supported versions

Only the latest release published at the environment's `get.*` host (`latest.json`) is
supported. Agents do not self-update: re-run the install command from the dashboard to
upgrade a host in place.
