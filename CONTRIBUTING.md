# Contributing

Use Go 1.25+, Node.js 22.12+, a C++17 compiler, PCRE2 development headers and pkg-config. Install frontend dependencies with `npm --prefix frontend ci`, then run `bash scripts/verify-all.sh`.

For changes to masking or restoration, include a regression that verifies the forwarded request and restored reply. Cover tool arguments and streaming boundaries when affected. Use synthetic inputs and disposable databases; keep real prompts, credentials and operational records out of commits.

The native engine is required. MySQL integration is opt-in locally via `SILENCER_TEST_MYSQL_DSN` and runs against a disposable MySQL service in CI. The Docker smoke test checks readiness, extension endpoint shutdown and persisted configuration across container recreation:

```bash
docker build -t silencer:test .
bash scripts/test-image.sh silencer:test
```

Frontend changes should preserve both language dictionaries. Run lint and the log/privacy tests, and check affected loading, error and small-screen states. The browser plugin and its dedicated UI are outside the public release.

Keep commits focused. Explain behavior changes and validation in pull requests. Include only sanitized reproductions in bug reports; remove credentials, personal data and sensitive conversation text.
