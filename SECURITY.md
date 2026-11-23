# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| Latest `0.x` minor | ✅ bug + security fixes |
| Previous `0.x` minor | 🔒 security fixes only |
| Older | ❌ |

## Reporting a vulnerability

**Please do not open public issues for security problems.**

Email **info@haio.ir** with:

- a description of the issue and its impact,
- steps to reproduce (proof-of-concept if possible),
- affected version(s) and configuration.

We will acknowledge within 72 hours and keep you informed of the fix timeline.
Once a fix is released we will publish an advisory and credit the reporter
(unless anonymity is requested).

## Scope notes

- vmlet manages KVM, TAP devices and cgroups and is intended to run on
  dedicated hosts behind a trusted control plane. The REST API has **no
  built-in end-user authentication** in `0.x` — never expose it directly to
  untrusted networks; front it with an authenticated gateway.
- Node-to-node migration uses a shared token; change the
  `CHANGE_ME_MIGRATION_TOKEN` default before any multi-host use. mTLS is on
  the roadmap (Phase 2).
