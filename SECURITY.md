# Security policy

GalleryGo 2 is under active development and is not yet a production release.
Do not expose development builds to the public Internet or use them to protect
sensitive media before the release security gates have passed.

## Reporting

Do not publish private media, credentials, exploit payloads containing user data,
or unredacted logs in an issue. Use the repository hosting provider's private
vulnerability reporting feature if enabled. If no private contact is available,
open an issue asking for a private reporting channel without disclosing the flaw.

## Security boundaries

- NAS originals are read-only and must be mounted read-only in deployments.
- Metadata databases and generated variants belong on separate private storage.
- Authentication, authorization, CSRF protection, and safe paths are server duties.
- Service workers and browser storage are not an authorization boundary against a
  compromised operating-system account or browser profile.
- Offline revocation cannot be immediate while a device is disconnected. The
  eventual offline implementation must enforce a limited grant lifetime and make
  this limitation explicit.
