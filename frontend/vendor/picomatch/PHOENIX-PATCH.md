# PHOENIX security patch

This is a local patch of upstream `picomatch` 4.0.7 for GHSA-fc5q-39xc-vjpf / CVE-2026-112366. It is based on the parser and regression-test changes proposed in upstream PR [micromatch/picomatch#259](https://github.com/micromatch/picomatch/pull/259), commit `fc648a5b4a4efc05a812b9662f999892533d9759`. The upstream PR has not been released as an npm version (the public registry still reports `4.0.7` as latest), so this PHOENIX-only fork is locally versioned `4.0.8`. This is not an upstream release; the local version is above the advisory's vulnerable range (`<=4.0.7`) so npm audit can distinguish the patched fork from the vulnerable upstream tarball.

The patch mitigates the reported repeated-extglob catastrophic-backtracking cases and adds conservative handling for nested/ambiguous repeated patterns. It does not assert that arbitrary user-controlled glob patterns are universally ReDoS-proof. The existing explicit `maxExtglobRecursion: false` opt-out disables the upstream safeguards and must not be used for attacker-controlled patterns.

The consuming app has a separate subprocess-timeout regression test at `src/test/picomatch-security.test.ts`. Reassess and replace this fork with an official upstream release once one containing a reviewed fix is published.
