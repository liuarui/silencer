# Publishing

Develop, validate, commit and publish from this checkout. The repository is
`https://github.com/liuarui/silencer`; preserve its existing history. A second
source checkout and export step are no longer used.

## Public source boundary

`scripts/public-release-files.txt` lists reviewed public files. Add new source,
tests and documentation after review. Run `python3 scripts/check-public-release.py`
before staging and again before pushing. Commit explicit reviewed paths.

Browser extension source, dedicated UI/guides and private development dictionaries
stay ignored locally. Runtime data, credentials, CA private keys, internal
operational records and `.private/` backups must never enter Git or Docker build
contexts. Local extension files are not needed by public builds.

Preserve the sponsor sections at the end of `README.md` and `README_EN.md`.
Update release text around them without deleting or replacing sponsor content.

## Release sequence

1. Update the gateway/frontend version, changelog and `docs/releases/<version>.md`.
2. Run the public-source check, frontend checks, native tests and installer tests.
3. Push the reviewed commit to `main`; wait for core/MySQL/image and native checks.
4. Tag the verified commit `v<version>` and push that exact tag. The release workflow
   builds native packages, verifies each Docker platform before pushing candidates,
   combines the verified digests, checks published manifests and publishes assets.
5. Verify GitHub Release assets, installation manifest and Docker tags. Never move
   an existing version tag to hide a failed release; fix the issue explicitly.

Docker Hub CI uses repository secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`.
Use a dedicated access token with read/write access, never an account password in
workflow source. GitHub Release creation uses `GITHUB_TOKEN` with job-scoped
`contents: write` permission. Pull requests do not receive publishing credentials
or publish artifacts as releases.

Images must contain both `linux/amd64` and `linux/arm64`. Each native runner checks
readiness, real C++ masking, persisted settings/password and disabled extension
endpoints before promotion. MySQL checks use an isolated disposable service.

Native capture packages cover Windows x64, Linux x64 and macOS Intel/Apple Silicon.
Build/smoke checks do not replace system-permission, UAC/eBPF, certificate-trust and
Clash/TUN field validation. State verified coverage and remaining limits in release
notes; do not claim untested combinations are fully protected.
