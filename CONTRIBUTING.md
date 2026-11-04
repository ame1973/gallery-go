# Contributing

GalleryGo is an MIT-licensed, NAS-first private gallery. Contributions must preserve
read-only source media and default-deny authorization.

## Change requirements

- Use English comments, documentation, and commit messages.
- Use [Conventional Commits](https://www.conventionalcommits.org/), for example
  `feat(auth): revoke sessions when a user is disabled`.
- Keep commits small and include regression tests for behavior changes.
- Update `contracts/openapi.yaml` and regenerate the TypeScript API types whenever
  an endpoint changes. Do not edit generated types by hand.
- Add a new numbered SQL migration for schema changes. Never edit a migration
  already used by a released version.
- Test against isolated temporary databases and synthetic media, not private NAS data.
- Do not commit credentials, local configuration, private media, development notes,
  or AI assistant instruction files.
- Never claim that mocked codecs, storage, or browsers prove real-device support.

The commands and deployment prerequisites are documented in [readme.md](readme.md).
