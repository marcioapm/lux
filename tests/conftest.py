"""pytest fixtures for the lux end-to-end suite."""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import pytest

from env import TestEnvironment


def _require(env: TestEnvironment, *binaries: str) -> None:
    missing = [b for b in binaries if not env.binaries.get(b)]
    if missing:
        pytest.skip(f"not built yet: {', '.join(missing)}")


@pytest.fixture(scope="session")
def env() -> TestEnvironment:
    """The environment run_tests.py set up."""
    return TestEnvironment.load()


@pytest.fixture(scope="session")
def hosts(env: TestEnvironment):
    return env.hosts


@pytest.fixture(scope="session")
def require(env: TestEnvironment):
    """`require("luxd", "lux-runner")` skips the test until those exist."""
    return lambda *b: _require(env, *b)
