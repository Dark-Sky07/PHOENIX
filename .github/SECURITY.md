# Security policy

## Supported releases

Security fixes are prepared for the latest stable PHOENIX release on `main`.
Older releases should be upgraded after the release notes and installation
requirements have been reviewed.

## Reporting a vulnerability

Please do **not** open a public issue for a suspected vulnerability, exposed
credential, authentication bypass, traffic-accounting error, or unsafe update
path.

Use GitHub Private Vulnerability Reporting instead:

https://github.com/Dark-Sky07/PHOENIX/security/advisories/new

Include a minimal reproduction, the affected version or commit, the deployment
shape needed to reproduce it, and the impact. Do not include real customer data,
passwords, API tokens, private keys, subscription URLs, database backups, or
production addresses in the report.

If Private Vulnerability Reporting is unavailable, contact the repository owner
privately through the contact method on the GitHub profile and state that the
report is security-sensitive. Do not publish exploit details while a fix is in
progress.

## Handling reports

Maintainers will acknowledge a report, reproduce it in an isolated environment,
and coordinate a fix before public disclosure. Credentials that may have been
exposed must be revoked or rotated immediately; removing a value from Git is not
sufficient because it may remain in history, releases, logs, or caches.

## Scope

This policy covers PHOENIX source, installers, updater scripts, release
artifacts, Docker images, CI workflows, and the panel's documented APIs. It does
not authorize testing against infrastructure, accounts, nodes, or networks that
you do not own or have explicit permission to test.
