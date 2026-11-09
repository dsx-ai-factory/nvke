# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the publish version rules in describe-version.sh against fixture repositories."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("describe-version.sh")
# A hook can export GIT_DIR and friends, which would point git at the outer
# repository instead of the fixture. The global and system configs stay out too,
# so a developer's commit and tag signing does not reach the fixture.
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_") and k != "GITHUB_REF"}
ENV.update(
    GIT_CONFIG_GLOBAL=os.devnull,
    GIT_CONFIG_NOSYSTEM="1",
    GIT_AUTHOR_NAME="fixture",
    GIT_AUTHOR_EMAIL="fixture@example.com",
    GIT_COMMITTER_NAME="fixture",
    GIT_COMMITTER_EMAIL="fixture@example.com",
)


class DescribeVersionTest(unittest.TestCase):
    def setUp(self):
        self.new_repository()

    def new_repository(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.git("init", "--quiet", "--initial-branch=main")

    def git(self, *args):
        result = subprocess.run(["git", *args], cwd=self.root, capture_output=True, text=True, check=True, env=ENV)
        return result.stdout.strip()

    def commit(self):
        self.git("commit", "--quiet", "--allow-empty", "--message", "chore: change")
        return self.git("rev-parse", "HEAD")

    def release(self, tag):
        # Releases are annotated; betas and RCs are lightweight.
        self.git("tag", "--annotate", "--message", tag, tag)

    def describe(self, ref=None):
        env = dict(ENV)
        if ref is not None:
            env["GITHUB_REF"] = ref
        return subprocess.run(["bash", str(SCRIPT)], cwd=self.root, capture_output=True, text=True, check=False, env=env)

    def assert_version(self, want, ref=None):
        result = self.describe(ref)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), want)

    def assert_refused(self, reason, ref=None):
        result = self.describe(ref)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout, "")
        self.assertIn(reason, result.stderr)

    def test_untagged_history_is_the_commit(self):
        sha = self.commit()
        self.assert_version(sha[:7], "refs/heads/main")

    def test_main_is_the_nearest_tag_described(self):
        self.commit()
        self.git("tag", "v0.2.0-beta.1")
        sha = self.commit()
        self.assert_version(f"v0.2.0-beta.1-1-g{sha[:7]}", "refs/heads/main")

    def test_release_run_is_its_tag(self):
        self.commit()
        self.git("tag", "v0.2.0-beta.1")
        self.release("v0.2.0")
        self.assert_version("v0.2.0", "refs/tags/v0.2.0")

    def test_beta_rerun_on_a_released_commit_keeps_the_beta(self):
        # git describe names the annotated release here.
        self.commit()
        self.git("tag", "v0.2.0-beta.1")
        self.release("v0.2.0")
        self.assert_version("v0.2.0", "refs/heads/main")
        self.assert_version("v0.2.0-beta.1", "refs/tags/v0.2.0-beta.1")

    def test_second_beta_on_one_commit_is_its_own_tag(self):
        # Of two lightweight tags, git describe names the first by name.
        self.commit()
        self.git("tag", "v0.2.0-beta.4")
        self.git("tag", "v0.2.0-beta.5")
        self.assert_version("v0.2.0-beta.4", "refs/heads/main")
        self.assert_version("v0.2.0-beta.5", "refs/tags/v0.2.0-beta.5")

    def test_tag_naming_another_commit_is_refused(self):
        self.commit()
        self.git("tag", "v0.2.0-beta.1")
        self.commit()
        self.assert_refused("does not name", "refs/tags/v0.2.0-beta.1")

    def test_tag_run_for_a_missing_tag_is_refused(self):
        self.commit()
        self.assert_refused("does not name", "refs/tags/v0.2.0-beta.1")

    def test_component_rc_on_a_release_is_refused(self):
        self.commit()
        self.release("v0.2.0")
        self.git("tag", "agent/v0.2.1-rc.1")
        self.assert_refused("refusing to republish v0.2.0", "refs/tags/agent/v0.2.1-rc.1")

    def test_component_rc_on_a_beta_is_refused(self):
        self.commit()
        self.git("tag", "v0.2.0-beta.1")
        self.git("tag", "agent/v0.2.0-rc.1")
        self.assert_refused("refusing to republish v0.2.0-beta.1", "refs/tags/agent/v0.2.0-rc.1")

    def test_component_rc_on_an_untagged_commit_is_described(self):
        self.commit()
        self.git("tag", "v0.2.0-beta.1")
        sha = self.commit()
        self.git("tag", "cli/v0.2.0-rc.1")
        self.assert_version(f"v0.2.0-beta.1-1-g{sha[:7]}", "refs/tags/cli/v0.2.0-rc.1")

    def test_tag_that_is_not_a_version_is_refused(self):
        for name in ("v0$(id)", "v0.2.0`id`", "v0.2.0;id", "v0.2.0-beta.1${IFS}x", "v0.2.0-alpha.1"):
            with self.subTest(name=name):
                self.new_repository()
                self.commit()
                self.release(name)
                self.assert_refused("not a version this repository publishes", "refs/heads/main")
                self.assert_refused("not a version this repository publishes", f"refs/tags/{name}")

    def test_bad_describe_is_refused_on_any_tag_run(self):
        # The Makefiles describe the commit themselves, so a v* run is refused
        # when the describe is unsafe, even though it is published under its tag.
        self.commit()
        self.git("tag", "v0.2.0-beta.1")
        self.release("v0$(id)")
        self.assert_refused("not a version this repository publishes", "refs/tags/v0.2.0-beta.1")


if __name__ == "__main__":
    unittest.main()
