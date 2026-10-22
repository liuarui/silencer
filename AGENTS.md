# Project workflow

Maintain this project directly in this checkout. Its `origin` is
`https://github.com/liuarui/silencer.git`; preserve that repository's history.
There is no separate development/export checkout.

Reviewed browser extension source and tests live in `extension/` and are published
with the project. Its dedicated console UI, guides and private development
dictionaries remain local and ignored.
Never commit runtime data, credentials, private CA keys, internal deployment
records, local tool state or `.private/` backups. Check reviewed source paths
with `python3 scripts/check-public-release.py` before publishing. Add new public
files to `scripts/public-release-files.txt` after review.

Read `docs/PUBLISHING.md` before a release. Run the required tests and wait for
the image and native-package workflows. Publish only verified image digests.
Keep README sponsor sections intact unless the user explicitly changes them.
