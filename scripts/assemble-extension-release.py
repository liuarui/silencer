#!/usr/bin/env python3
"""Package the locally ignored browser extension as a reproducible release asset."""
from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path
import zipfile

ROOT = Path(__file__).resolve().parents[1]
FILES = (
    "manifest.json",
    "background.js",
    "bridge-isolated.js",
    "bridge-main.js",
    "options.html",
    "options.js",
    "popup.html",
    "popup.js",
    "shared.js",
    "webchat-protocols.js",
)
SUPPORTED_SITES = {"chatgpt.com", "gemini.google.com", "deepseek.com"}
FORBIDDEN_MARKERS = (
    "__silencer_chatgpt_response",
    "debug_passthrough",
    "inspectChatgptSSE",
    "webchat-p3-text13",
)

INSTALL = """Silencer Web Chat {version}

License: GNU AGPL-3.0-only (see LICENSE).
Supported sites: ChatGPT, Gemini and DeepSeek text conversations.
File uploads, images, audio, WebSocket and other sites are not covered.

Install in Chrome or Chromium:
1. Extract this ZIP to a folder that you will keep on this computer.
2. Open chrome://extensions and enable Developer mode.
3. Choose Load unpacked and select the extracted folder.
4. Open the Silencer extension settings. Enter the gateway URL and extension token.
5. Grant access to the sites you use, then reload already-open AI tabs.

The Silencer gateway's browser-extension integration must be enabled. Use only a
gateway you trust: the extension sends masked conversation text to that gateway.
"""


def fail(message: str) -> None:
    raise SystemExit(message)


def safe_read(source: Path, relative: str) -> bytes:
    path = source / relative
    if path.is_symlink() or not path.is_file():
        fail(f"Missing, non-file or symlinked extension asset: {relative}")
    return path.read_bytes()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="Stable release version, e.g. 1.1.2")
    parser.add_argument("--source", type=Path, default=ROOT / "extension", help="Local ignored extension source")
    parser.add_argument("--output", type=Path, help="ZIP destination (default: dist_extension/silencer-webchat-VERSION.zip)")
    args = parser.parse_args()

    if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
        fail("Version must use MAJOR.MINOR.PATCH format")
    source = args.source.resolve()
    if not source.is_dir():
        fail(f"Extension source directory not found: {source}")
    output = args.output or ROOT / "dist_extension" / f"silencer-webchat-{args.version}.zip"
    output = output.resolve()
    if output == source or source in output.parents:
        fail("Output must be outside the extension source directory")

    contents = {name: safe_read(source, name) for name in FILES}
    try:
        manifest = json.loads(contents["manifest.json"])
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(f"Invalid extension manifest: {exc}")
    app_version = json.loads((ROOT / "frontend/package.json").read_text(encoding="utf-8"))["version"]
    if args.version != app_version:
        fail(f"Package version {args.version} must match Silencer version {app_version}")
    if (manifest.get("manifest_version") != 3 or manifest.get("version") != args.version
            or manifest.get("version_name") != args.version):
        fail(f"Manifest must be MV3 and version/version_name {args.version}")

    shared = contents["shared.js"].decode("utf-8")
    match = re.search(r"const\s+STATIC_SITES\s*=\s*\[([^]]*)\]", shared)
    if not match:
        fail("Could not locate STATIC_SITES in shared.js")
    shared_sites = set(re.findall(r"['\"]([a-z0-9.-]+)['\"]", match.group(1)))
    manifest_sites: set[str] = set()
    for script in manifest.get("content_scripts", []):
        for pattern in script.get("matches", []):
            parsed = re.fullmatch(r"\*://\*\.([a-z0-9.-]+)/\*", pattern)
            if not parsed:
                fail(f"Unexpected static content-script match pattern: {pattern}")
            manifest_sites.add(parsed.group(1))
    if shared_sites != SUPPORTED_SITES or manifest_sites != SUPPORTED_SITES:
        fail(f"Supported-site mismatch; shared={sorted(shared_sites)}, manifest={sorted(manifest_sites)}")
    webchat_block = re.search(r"const\s+WEBCHAT_SITES\s*=\s*\[(.*?)\n\s*\];", shared, re.S)
    preset_block = re.search(r"const\s+PRESET_SITES\s*=\s*\[(.*?)\n\s*\];", shared, re.S)
    webchat_ids = set(re.findall(r"\bid:\s*['\"]([a-z0-9_-]+)['\"]", webchat_block.group(1))) if webchat_block else set()
    preset_domains = set(re.findall(r"\bdomain:\s*['\"]([a-z0-9.-]+)['\"]", preset_block.group(1))) if preset_block else set()
    expected_ids = {"chatgpt", "gemini", "deepseek"}
    if webchat_ids != expected_ids or preset_domains != SUPPORTED_SITES:
        fail(f"Visible extension sites mismatch; adapters={sorted(webchat_ids)}, presets={sorted(preset_domains)}")

    for name, data in contents.items():
        text = data.decode("utf-8")
        for marker in FORBIDDEN_MARKERS:
            if marker in text:
                fail(f"Development-only marker {marker!r} found in {name}")

    files = dict(contents)
    files["LICENSE"] = (ROOT / "LICENSE").read_bytes()
    files["INSTALL.txt"] = INSTALL.format(version=args.version).encode("utf-8")
    output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, data in files.items():
            info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o100644 << 16
            archive.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)

    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    checksum = output.with_name(output.name + ".sha256")
    checksum.write_text(f"{digest}  {output.name}\n", encoding="ascii")
    with zipfile.ZipFile(output) as archive:
        names = archive.namelist()
        if names != [*FILES, "LICENSE", "INSTALL.txt"]:
            fail(f"Unexpected archive contents: {names}")
        bad_file = archive.testzip()
        if bad_file:
            fail(f"Archive CRC check failed: {bad_file}")
    print(f"Created {output.relative_to(ROOT) if ROOT in output.parents else output}")
    print(f"SHA-256 {digest}")
    print(f"Archive files: {', '.join(files)}")


if __name__ == "__main__":
    main()
