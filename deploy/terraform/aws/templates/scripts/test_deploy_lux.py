#!/usr/bin/env python3
"""Unit tests for deploy-lux.py's version-switch and rollback logic.

Runs entirely offline against a temp directory standing in for
/usr/local/lux; systemctl and the health check are stubbed via the
functions' own hooks (runner=, sleep=, now=), never the real subprocess
or network. Run with: python3 -m unittest test_deploy_lux.py
"""
import importlib.util
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

MODULE_PATH = os.path.join(os.path.dirname(__file__), "deploy-lux.py")
spec = importlib.util.spec_from_file_location("deploy_lux", MODULE_PATH)
deploy_lux = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deploy_lux)


def fake_run(returncode: int, stderr: str = "", stdout: str = ""):
    """A stand-in for subprocess.run that ignores its arguments and
    returns a fixed result, so run_migrate/restart_luxd/is_luxd_active
    can be tested without touching systemctl or luxd."""

    def runner(*args, **kwargs):
        return subprocess.CompletedProcess(args, returncode, stdout=stdout, stderr=stderr)

    return runner


class SymlinkSwitchTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="lux-deploy-test-")
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        self.bin_dir = os.path.join(self.root, "bin")
        self.runner_bin_dir = os.path.join(self.root, "runner")
        self.current_link = os.path.join(self.root, "current")

        self.old_version_dir = os.path.join(self.root, "versions", "v1")
        self.new_version_dir = os.path.join(self.root, "versions", "v2")
        for d in (self.old_version_dir, self.new_version_dir):
            os.makedirs(os.path.join(d, "bin"))
            os.makedirs(os.path.join(d, "lib", "lux", "runner"))
            open(os.path.join(d, "bin", "luxd"), "w").close()
            open(os.path.join(d, "bin", "lux"), "w").close()

        # Simulate an already-deployed v1: current and the stable links
        # all point into old_version_dir, as switch_symlinks would have
        # left them.
        self.links = deploy_lux.stable_links(self.current_link, self.bin_dir, self.runner_bin_dir)
        os.symlink(self.old_version_dir, self.current_link)
        for link_path, target in self.links.items():
            os.makedirs(os.path.dirname(link_path), exist_ok=True)
            os.symlink(os.path.join(self.old_version_dir, os.path.relpath(target, self.current_link)), link_path)

    def target_of(self, link_path: str) -> str:
        return os.path.realpath(link_path)

    def test_switch_points_every_link_at_the_new_version(self):
        deploy_lux.switch_symlinks(self.new_version_dir, self.current_link, self.links)
        self.assertEqual(self.target_of(self.current_link), self.new_version_dir)
        for link_path in self.links:
            self.assertTrue(self.target_of(link_path).startswith(self.new_version_dir + os.sep))

    def test_restore_after_switch_returns_every_link_to_v1(self):
        previous = deploy_lux.switch_symlinks(self.new_version_dir, self.current_link, self.links)
        deploy_lux.restore_symlinks(previous, self.current_link, self.links)
        self.assertEqual(self.target_of(self.current_link), self.old_version_dir)
        for link_path in self.links:
            self.assertTrue(self.target_of(link_path).startswith(self.old_version_dir + os.sep))

    def test_restore_with_no_previous_deploy_removes_the_links(self):
        # A first-ever deploy: nothing existed before switch_symlinks ran.
        for link_path in [self.current_link, *self.links]:
            os.remove(link_path)
        previous = {self.current_link: None, **{k: None for k in self.links}}
        deploy_lux.switch_symlinks(self.new_version_dir, self.current_link, self.links)
        deploy_lux.restore_symlinks(previous, self.current_link, self.links)
        self.assertFalse(os.path.islink(self.current_link))
        for link_path in self.links:
            self.assertFalse(os.path.islink(link_path))


class ExtractReleaseTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="lux-deploy-test-")
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)

    def _make_tarball(self, version: str) -> str:
        import tarfile

        src = os.path.join(self.root, "src")
        os.makedirs(os.path.join(src, "bin"), exist_ok=True)
        with open(os.path.join(src, "bin", "luxd"), "w") as f:
            f.write(version)
        tarball_path = os.path.join(self.root, f"{version}.tar.gz")
        with tarfile.open(tarball_path, "w:gz") as tf:
            tf.add(src, arcname=".")
        shutil.rmtree(src)
        return tarball_path

    def test_extract_never_touches_the_currently_running_version_dir(self):
        versions_root = os.path.join(self.root, "versions")
        old_dir = deploy_lux.extract_release(self._make_tarball("v1"), versions_root, "v1")
        marker = os.path.join(old_dir, "bin", "luxd")
        self.assertTrue(os.path.exists(marker))

        new_dir = deploy_lux.extract_release(self._make_tarball("v2"), versions_root, "v2")

        self.assertTrue(os.path.exists(marker), "extracting v2 deleted v1's files")
        with open(os.path.join(new_dir, "bin", "luxd")) as f:
            self.assertEqual(f.read(), "v2")

    def test_extract_replaces_a_stale_partial_directory_for_the_same_version(self):
        versions_root = os.path.join(self.root, "versions")
        stale = os.path.join(versions_root, "v1")
        os.makedirs(stale)
        with open(os.path.join(stale, "leftover"), "w") as f:
            f.write("partial")

        version_dir = deploy_lux.extract_release(self._make_tarball("v1"), versions_root, "v1")
        self.assertFalse(os.path.exists(os.path.join(version_dir, "leftover")))
        self.assertTrue(os.path.exists(os.path.join(version_dir, "bin", "luxd")))


class MigrateAndRestartTest(unittest.TestCase):
    def test_run_migrate_reports_success(self):
        ok, _ = deploy_lux.run_migrate("/bin/true", "postgres://x", runner=fake_run(0))
        self.assertTrue(ok)

    def test_run_migrate_reports_failure_and_stderr(self):
        ok, stderr = deploy_lux.run_migrate("/bin/false", "postgres://x", runner=fake_run(1, stderr="boom"))
        self.assertFalse(ok)
        self.assertEqual(stderr, "boom")

    def test_restart_luxd_reports_failure(self):
        ok, stderr = deploy_lux.restart_luxd(runner=fake_run(1, stderr="unit not found"))
        self.assertFalse(ok)
        self.assertEqual(stderr, "unit not found")

    def test_is_luxd_active_reads_systemctl_stdout(self):
        self.assertTrue(deploy_lux.is_luxd_active(runner=fake_run(0, stdout="active\n")))
        self.assertFalse(deploy_lux.is_luxd_active(runner=fake_run(3, stdout="failed\n")))


class WaitHealthyTest(unittest.TestCase):
    def test_returns_true_as_soon_as_both_checks_pass(self):
        calls = {"n": 0}

        def check_active():
            calls["n"] += 1
            return calls["n"] >= 2  # active only from the 2nd poll onward

        ok = deploy_lux.wait_healthy(
            check_active,
            lambda: True,
            timeout_s=10,
            interval_s=1,
            sleep=lambda s: None,
            now=lambda: 0,
        )
        self.assertTrue(ok)
        self.assertEqual(calls["n"], 2)

    def test_returns_false_once_the_deadline_passes(self):
        clock = {"t": 0}

        def now():
            return clock["t"]

        def sleep(s):
            clock["t"] += s

        ok = deploy_lux.wait_healthy(
            lambda: False,
            lambda: False,
            timeout_s=5,
            interval_s=2,
            sleep=sleep,
            now=now,
        )
        self.assertFalse(ok)


if __name__ == "__main__":
    unittest.main()
