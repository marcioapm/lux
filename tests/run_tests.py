#!/usr/bin/env python3
"""Entry point for the lux end-to-end suite.

    uv run python run_tests.py                   # build, bring everything up, run all suites
    uv run python run_tests.py --infra-only      # only check the environment itself
    uv run python run_tests.py suites/test_x.py  # one suite; -x, -k work as in pytest
    uv run python run_tests.py --keep            # keep containers and database to debug
    uv run python run_tests.py --hosts 3         # more simulated hosts
    uv run python run_tests.py --real-ec2        # EC2 suites against real AWS (nightly)
    uv run python run_tests.py --serve           # a lux to develop against (see serve.py)

Each invocation gets its own database, bucket, Docker network and hosts, so
runs do not collide with each other or with a development instance.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from build import build_agent_images, build_binaries, build_fake_image, build_nested_image  # noqa: E402
from env import TestEnvironment  # noqa: E402

TESTS_DIR = Path(__file__).resolve().parent


def main() -> None:
    parser = argparse.ArgumentParser(description="Run the lux end-to-end suite")
    parser.add_argument("--infra-only", action="store_true", help="only check the environment itself")
    parser.add_argument("--keep", action="store_true", help="keep containers and database after the run")
    parser.add_argument("--hosts", type=int, default=2, help="number of simulated hosts")
    parser.add_argument("--real-ec2", action="store_true", help="run EC2 suites against real AWS")
    parser.add_argument("--serve", action="store_true", help="bring lux up with runners and a tenant, and keep it up")
    parser.add_argument("--detach", action="store_true", help="with --serve: return once it is up")
    parser.add_argument("--down", nargs="?", const="", metavar="ENV_JSON", help="take a detached --serve down")
    parser.add_argument("--image", action="append", default=[], metavar="REF",
                        help="with --serve: preload an image from the local Docker into every host (repeatable)")
    args, pytest_args = parser.parse_known_args()

    if args.down is not None:
        from serve import down
        down(args.down or None)
        return

    print("building:")
    built = build_binaries()
    fake_image = build_fake_image(built.get("lux-fake"))
    # Agent images are big: build (and ship to hosts) only when their suite runs.
    agent_images = build_agent_images(any("agents" in a for a in pytest_args))

    env = TestEnvironment(n_hosts=args.hosts)
    env.binaries = {k: str(v) if v else None for k, v in built.items()}
    # The nested image is only for its suite (a full run, or one naming it).
    selected = [a for a in pytest_args if not a.startswith("-")]
    nested = build_nested_image() if not selected or any("nested" in a for a in selected) else None
    env.extra["images"] = {**agent_images, "nested": nested, **{ref: ref for ref in args.image}}
    print(f"run id:   {env.run_id}")
    print(f"logs:     {env.log_dir}")

    if args.serve:
        from serve import serve
        try:
            serve(env, fake_image, args.detach)
        except BaseException:
            env.teardown()
            raise
        return

    exit_code = 1
    try:
        env.setup(fake_image=fake_image, luxd=not args.infra_only)
        print(f"network:  {env.network} ({env.subnet}, gateway {env.gateway})")
        print(f"hosts:    {', '.join(f'{h.name}={h.ip}' for h in env.hosts)}")
        if env.binaries.get("luxd") and not args.infra_only:
            print(f"luxd:     {env.luxd_url}")
        print()

        os.environ["LUX_TEST_ENV"] = str(env.env_file)
        if args.real_ec2:
            os.environ["LUX_TEST_REAL_EC2"] = "1"

        if args.infra_only:
            pytest_args += ["-m", "infra"]
        if not any(not a.startswith("-") for a in pytest_args if a not in ("infra",)):
            pytest_args = [str(TESTS_DIR / "suites"), *pytest_args]

        result = subprocess.run([sys.executable, "-m", "pytest", *pytest_args], cwd=TESTS_DIR)
        exit_code = result.returncode
    finally:
        env.teardown(keep=args.keep)
        print(f"\nlogs: {env.log_dir}")
        if args.keep:
            print(f"environment kept — network {env.network}, database {env.db_name}, bucket {env.bucket}")

    sys.exit(exit_code)


if __name__ == "__main__":
    main()
