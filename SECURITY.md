# Security

## Reporting a vulnerability

Report privately through GitHub's
[security advisories](https://github.com/bakhod1r/seedora/security/advisories/new),
not in the issue tracker.

Please include what an attacker gains, the steps to reproduce it, and the
version and engine you saw it on. You will get an acknowledgement within three
working days and an assessment within ten. A fix ships in a patch release, with
credit unless you would rather not have it.

## What is in scope

Seedora is a developer tool that runs on a developer's machine and connects to
a developer's database. That shapes what counts:

- **Credential exposure.** A DSN carries a password. It must never reach the
  browser, `seedora.yaml`, a log line, or an error message. Remembered
  connections live in the user's config directory, readable only by them, and
  the password is stored only when explicitly asked for.
- **Writing where it should not.** The production guard, the truncate
  confirmation, the SQL preview, and the dry run all exist to keep the tool
  from writing somewhere unintended. A path around one of them is a
  vulnerability.
- **SQL injection through identifiers.** Table and column names reach generated
  SQL. They are quoted, and new identifiers are validated. A name that escapes
  either is a vulnerability.
- **The local HTTP server.** It binds to `127.0.0.1` by default and holds an
  open database connection. Anything that lets another origin drive it — CSRF,
  DNS rebinding, a missing origin check — is in scope.

## What is not

- **Binding the UI to a public interface.** `SEEDORA_HOST` allows it because
  some people run this in a container. There is no authentication, and a
  Seedora instance reachable from a network is a database reachable from that
  network. That is the operator's decision, and it is documented as such.
- **Bypassing the production guard with `--i-know-what-im-doing`.** The flag is
  spelled the way it is on purpose.
- **Generated data being predictable.** Seeding is deterministic by design:
  `--seed` produces the same rows twice, and that is a feature. Seedora's
  generators are not a source of secrets, and nothing they produce should be
  treated as one.
- **Anything requiring an attacker who already has the ability to run code as
  the user.** At that point they have the database.

## Supported versions

Until `1.0.0`, only the latest release gets fixes.
