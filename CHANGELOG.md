# Changelog

All notable public changes are recorded here. This project uses Conventional Commits.

## Unreleased

### Added

- One-time administrator bootstrap with hidden terminal or explicit stdin password
  input, bcrypt cost 12, hashed 256-bit sessions, persistent authentication rate
  limits, atomic security events, and bounded session lifetimes.

- Local SQLite WAL/FTS5 storage and atomic, ordered, checksummed migrations.
- Strict YAML configuration with separate source, database, cache, and web roots.
- Go CLI for migration, serving, database diagnosis, health checks, and version output.
- Public health/config APIs, Problem JSON errors, security headers, and protected
  static-file boundaries without a public media mount.
- React/TypeScript connection page with English, Traditional Chinese, and Simplified
  Chinese, self-hosted fonts, generated API types, and real-browser tests.
- Digest-pinned rootless Docker foundation, deployment instructions, and CI checks.

### Compatibility

- This is a new implementation. Old GalleryGo databases and API routes are not supported.
- Private gallery features are not yet available; this is not a GA release.
- TypeScript is pinned to 6.0.3 because the selected ESLint toolchain does not yet
  accept TypeScript 7. All direct dependencies use exact versions and lockfiles.
