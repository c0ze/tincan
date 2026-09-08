# Documentation

Use these guides for tincan v2:

- [README](../README.md): installation and quick start.
- [Client and provider setup](setup.md): MCP configuration, supported workers,
  session prompts and authentication troubleshooting.
- [Operating protocol](../PROTOCOL.md): CLI and MCP arguments, delivery,
  persistence, cancellation, trust boundary and platform limits.
- [v2.0.0 release notes](releases/v2.0.0.md): changes, upgrade steps and validation.
- [Contributor guide](contributing.md): local checks and release process.
- [Tell skill](../skills/tell/SKILL.md) and
  [listen skill](../skills/listen/SKILL.md): instructions installed into agent clients.
- [Gemini command](../skills/gemini/listen.toml): the installed `/listen` shim.

## Historical records

These records capture decisions and verification at a point in time. Their
version numbers, module paths, proposed commands and implementation plans are
historical; use the guides above for current behavior.

- [September stability and MCP validation](reviews/2026-09-08-stability-mcp-validation.md),
  including subsequent native CI and live provider checks.
- [July Antigravity documentation review](reviews/2026-07-03-antigravity-docs-review.md).
- [July Codex phase-one review](reviews/2026-07-03-codex-phase1-review.md).
- [Original design](superpowers/specs/2026-07-03-tincan-design.md) and
  [phase-one implementation plan](superpowers/plans/2026-07-03-tincan-phase1.md).
- [Hosted-listener design](superpowers/specs/2026-09-07-hosted-listeners-design.md)
  and [implementation plan](superpowers/plans/2026-09-07-hosted-listeners.md).
- [Legacy Codex prompt shim](../skills/codex/listen.md), retained for reference;
  the installer now uses native skills.
