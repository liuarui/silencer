# Publishing

Develop, validate, commit and publish from this checkout. The repository is
`https://github.com/liuarui/silencer`; preserve its existing history. A second
source checkout and export step are no longer used.

## Public source boundary

`scripts/public-release-files.txt` lists reviewed public files. Add new source,
tests and documentation after review. Run `python3 scripts/check-public-release.py`
before staging and again before pushing. Commit explicit reviewed paths.

The reviewed browser extension source and tests live in `extension/` and are
published in Git. Dedicated console UI/guides and private development dictionaries
stay ignored locally. Runtime data, credentials, CA private keys, internal
operational records and `.private/` backups must never enter Git or Docker build
contexts.

Preserve the sponsor sections at the end of `README.md` and `README_EN.md`.
Update release text around them without deleting or replacing sponsor content.

## Release sequence

1. Update the gateway/frontend version, changelog and `docs/releases/<version>.md`.
2. Run the public-source check, frontend checks, native tests and installer tests.
3. Push the reviewed commit to `main`; wait for core/MySQL/image and native checks.
4. Tag the verified commit `v<version>` and push that exact tag. The release workflow
   builds native packages, verifies each Docker platform before pushing candidates,
   combines the verified digests, checks published manifests and publishes assets.
5. The release workflow packages the reviewed `extension/` source into a separate
   browser-extension ZIP and SHA-256 asset, then publishes them alongside native
   packages. The extension source is part of GitHub source releases, but it is not
   copied into Docker build contexts.
6. Verify GitHub Release assets, installation manifest and Docker tags. Never move
   an existing version tag to hide a failed release; fix the issue explicitly.

The extension packager accepts only an explicit runtime file allowlist, validates the
manifest version and supported-site lists, excludes tests and development bypasses,
and produces a stable ZIP plus SHA-256 checksum. CI builds the ZIP from the exact
tagged source; do not substitute a stale or unreviewed archive.

Docker Hub CI uses repository secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`.
Use a dedicated access token with read/write access, never an account password in
workflow source. GitHub Release creation uses `GITHUB_TOKEN` with job-scoped
`contents: write` permission. Pull requests do not receive publishing credentials
or publish artifacts as releases.

Images must contain both `linux/amd64` and `linux/arm64`. Each native runner checks
readiness, real C++ masking, persisted settings/password and the extension hard-disable
switch before promotion. Image smoke checks that the bridge is off by default and
rejects unauthenticated extension requests. MySQL checks use an isolated disposable
service.

Native capture packages cover Windows x64, Linux x64 and macOS Intel/Apple Silicon.
Build/smoke checks do not replace system-permission, UAC/eBPF, certificate-trust and
Clash/TUN field validation. State verified coverage and remaining limits in release
notes; do not claim untested combinations are fully protected.
