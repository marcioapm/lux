"""Build the artifacts the suite drives.

Every `cmd/<name>` that exists is built into `bin/`, statically: the binaries
run inside Fedora hosts, and the shim runs inside arbitrary images. A binary
whose package does not exist yet is reported, not fatal — suites that need it
skip.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
BIN_DIR = REPO_ROOT / "bin"
TESTS_DIR = REPO_ROOT / "tests"

BINARIES = ["luxd", "lux-runner", "lux-shim", "lux", "lux-fake"]

FAKE_IMAGE = "localhost/lux-fake:test"
CLAUDE_IMAGE = "localhost/lux-claude:test"


def build_binaries() -> dict[str, Path | None]:
    """Build each binary that has a package. Returns name → path or None."""
    BIN_DIR.mkdir(exist_ok=True)
    built: dict[str, Path | None] = {}
    if not (REPO_ROOT / "go.mod").exists():
        for name in BINARIES:
            print(f"  {name:12} missing (no go.mod yet)")
            built[name] = None
        return built

    for name in BINARIES:
        pkg = REPO_ROOT / "cmd" / name
        if not pkg.is_dir():
            print(f"  {name:12} missing (no cmd/{name})")
            built[name] = None
            continue
        out = BIN_DIR / name
        result = subprocess.run(
            ["go", "build", "-trimpath", "-o", str(out), f"./cmd/{name}"],
            cwd=REPO_ROOT,
            env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux"},
        )
        if result.returncode != 0:
            print(f"go build failed for {name}", file=sys.stderr)
            sys.exit(1)
        print(f"  {name:12} built")
        built[name] = out
    return built


def build_fake_image(fake_binary: Path | None) -> str | None:
    """Build the fake agent's image from a context holding only the binary.

    Not the repository: a context that large makes every build slow, and the
    image needs nothing else.
    """
    if fake_binary is None:
        return None
    with tempfile.TemporaryDirectory() as ctx:
        shutil.copy(fake_binary, Path(ctx) / "lux-fake")
        shutil.copy(TESTS_DIR / "images" / "fake" / "Containerfile", Path(ctx) / "Containerfile")
        result = subprocess.run(
            ["docker", "build", "-q", "-t", FAKE_IMAGE, "-f", "Containerfile", "."],
            cwd=ctx,
            stdout=subprocess.DEVNULL,
        )
    if result.returncode != 0:
        print("fake image build failed", file=sys.stderr)
        sys.exit(1)
    print(f"  {FAKE_IMAGE} built")
    return FAKE_IMAGE


def build_claude_image() -> str | None:
    """Claude Code for the opt-in real-agent suite: only when a key is set
    and claude is installed here."""
    import os
    if not os.environ.get("LUX_TEST_ANTHROPIC_API_KEY"):
        return None
    claude = shutil.which("claude")
    if not claude:
        print("  claude not installed; real-agent suite will skip")
        return None
    with tempfile.TemporaryDirectory() as ctx:
        shutil.copy(os.path.realpath(claude), Path(ctx) / "claude")
        shutil.copy(TESTS_DIR / "images" / "claude" / "Containerfile", Path(ctx) / "Containerfile")
        result = subprocess.run(["docker", "build", "-q", "-t", CLAUDE_IMAGE, "-f", "Containerfile", "."], cwd=ctx, stdout=subprocess.DEVNULL)
    if result.returncode != 0:
        print("claude image build failed", file=sys.stderr)
        sys.exit(1)
    print(f"  {CLAUDE_IMAGE} built")
    return CLAUDE_IMAGE
