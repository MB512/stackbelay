# Security policy

## Reporting a vulnerability

Please do **not** open a public issue for security problems. Use GitHub's
private vulnerability reporting instead: on the repository page, go to
**Security** -> **Report a vulnerability**.

Include what you found, how to reproduce it and which version
(shown in the page footer when logged in, and in the manager's start log)
is affected. You will get an answer as soon as possible; fixes are
released as a new version.

## Supported versions

Only the latest release receives security fixes.

## Scope

StackVault controls Docker through the Docker socket, which is root-equivalent
on the host. The threat model and the rules the code follows are described in
[`docs/security.md`](docs/security.md). Findings where an authenticated web
user can do more than their rights allow (other stacks, commands, files,
credentials) or an unauthenticated user can do anything at all are in scope.
