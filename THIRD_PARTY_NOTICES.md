# Third-party notices

Silencer's original code is licensed under AGPL-3.0-only; see [LICENSE](LICENSE).
Third-party components and assets retain their own licenses.

## Distributed frontend assets

| Component | License | Notice |
| --- | --- | --- |
| Inter variable font | SIL Open Font License 1.1 | [OFL-Inter.txt](frontend/src/assets/fonts/OFL-Inter.txt); copyright 2020 The Inter Project Authors |
| JetBrains Mono Regular font | SIL Open Font License 1.1 | [OFL-JetBrainsMono.txt](frontend/src/assets/fonts/OFL-JetBrainsMono.txt); copyright 2020 The JetBrains Mono Project Authors |
| Lucide React icons | ISC; some icons derive from Feather under MIT | [LUCIDE-ISC.txt](licenses/LUCIDE-ISC.txt) |

## Frontend packages

Dependencies are declared in [frontend/package.json](frontend/package.json) and
locked in [frontend/package-lock.json](frontend/package-lock.json). The direct
runtime dependencies are:

| License | Packages |
| --- | --- |
| MIT | `@radix-ui/react-dialog`, `@radix-ui/react-label`, `@radix-ui/react-select`, `@radix-ui/react-slot`, `@radix-ui/react-switch`, `@radix-ui/react-tabs`, `@radix-ui/react-tooltip`, `@tanstack/react-query`, `@tanstack/react-virtual`, `clsx`, `dayjs`, `react`, `react-dom`, `react-router-dom`, `tailwind-merge`, `zustand` |
| Apache-2.0 | `class-variance-authority` |
| ISC | `lucide-react` |

Direct development dependencies are `@types/node`, `@types/react`,
`@types/react-dom`, `@vitejs/plugin-react`, `autoprefixer`, `oxlint`, `postcss`,
`tailwindcss`, and `vite` (MIT), plus `typescript` (Apache-2.0). These packages
remain under their respective upstream licenses; the lockfile also records
license metadata for transitive packages.

## Gateway packages

The rewritten runtime declares Go dependencies in
[gateway/go.mod](gateway/go.mod). They are linked into the gateway and retain
their upstream licenses:

| Package | License |
| --- | --- |
| github.com/mattn/go-sqlite3 | MIT |
| github.com/go-sql-driver/mysql | MPL-2.0 |
| mvdan.cc/sh/v3 | BSD-3-Clause; [copyright and license](licenses/MVDAN-SH-BSD-3-CLAUSE.txt) |
| go.opentelemetry.io/otel, otel/trace, otel/sdk, otel/exporters/otlp/otlptrace/otlptracehttp | Apache-2.0; [license](licenses/OPENTELEMETRY-APACHE-2.0.txt) |

The C++ engine uses the C++17 standard library and PCRE2 (BSD-3-Clause), linked
as the distribution's `libpcre2-8` shared library. Its copyright and license are
installed by the OS package under `/usr/share/doc/libpcre2-8-0/copyright` in the
container. Pattern syntax validation also uses the C++ standard library.
This table covers direct runtime dependencies, not every
transitive package. Consult module and distribution metadata for complete
copyright and license terms when redistributing dependency bundles.

## Native capture bundles

Native releases include mitmproxy 12.2.3 (MIT) and its platform helpers. macOS
and Windows use the complete official standalone distribution, retaining vendor
notices and macOS signatures. Linux builds from the hash-pinned wheel lockfile;
the runtime includes installed package license files, the lockfile, installation
report and build manifest. mitmproxy-rs and platform packages are pinned to
0.12.11. Windows helper files include WinDivert, whose upstream license notices
remain part of the vendor distribution. See https://mitmproxy.org/ and
https://github.com/mitmproxy/mitmproxy for upstream source and notices.

PCRE2 10.47 is statically linked in macOS native packages with its LICENCE notice.
Windows includes MinGW runtime DLLs and PCRE2; their redistribution terms remain
applicable. Native build inputs and complete package contents should be reviewed
when redistributing modified bundles.
