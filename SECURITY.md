# Security policy

## Supported versions

We patch security issues on the latest tagged release.  Pre-1.0 releases
are supported only on the `main` branch.

## Reporting a vulnerability

Email `security@observe.dev` with a description of the issue, a
reproducer, and the affected version.  Please do **not** file a public
GitHub issue for security reports.

We aim to acknowledge reports within 72 hours and ship a fix within 14
days for critical findings.  If you would like public credit, say so in
your report — we credit reporters in release notes unless asked to
withhold.

## Scope

In scope:

- The Observe server binary and Go packages under `internal/`.
- The official SDKs under `sdk/`.
- The Neutron and Nucleus components when used as distributed with
  Observe.

Out of scope:

- Self-hosted installations running on untrusted networks without TLS.
- Deliberate misconfigurations (for example, disabling authentication
  via an undocumented env var).
- Vulnerabilities in third-party dependencies that have already been
  disclosed upstream; file those with the upstream project.
