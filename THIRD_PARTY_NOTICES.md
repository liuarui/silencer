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

## Python packages

The backend declares runtime dependencies in
[backend/pyproject.toml](backend/pyproject.toml); [requirements.txt](requirements.txt)
installs that package. NER support is an optional `ner` extra, not a required
runtime dependency. These packages are installed as separate distributions and
retain their upstream licenses:

| Package | License |
| --- | --- |
| Flask | BSD-3-Clause |
| mitmproxy | MIT |
| PyMySQL | MIT |
| onnxruntime (optional `ner` extra) | MIT |
| tokenizers (optional `ner` extra) | Apache-2.0 |

This table covers direct backend dependencies, not every transitive package.
Each dependency remains under its own license and notice; consult the package
distribution metadata and upstream project for complete copyright and license
terms when redistributing dependency bundles.
