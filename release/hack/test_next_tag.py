# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the release version logic in next-tag.sh against fixture repositories."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("next-tag.sh")
# A hook can export GIT_DIR and friends, which would point git at the outer
# repository instead of the fixture. The global and system configs stay out too,
# so a developer's commit signing does not reach the fixture's commits.
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
ENV.update(
    GIT_CONFIG_GLOBAL=os.devnull,
    GIT_CONFIG_NOSYSTEM="1",
    GIT_AUTHOR_NAME="fixture",
    GIT_AUTHOR_EMAIL="fixture@example.com",
    GIT_COMMITTER_NAME="fixture",
    GIT_COMMITTER_EMAIL="fixture@example.com",
)
NOTHING_RELEASABLE = 3


class NextTagTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.git("init", "--quiet", "--initial-branch=main")
        self.commit("chore: start")
        self.git("tag", "v0.22.0")

    def git(self, *args):
        result = subprocess.run(["git", *args], cwd=self.root, capture_output=True, text=True, check=True, env=ENV)
        return result.stdout.strip()

    def commit(self, *subjects):
        for subject in subjects:
            self.git("commit", "--quiet", "--allow-empty", "--message", subject)
        return self.git("rev-parse", "HEAD")

    def next_tag(self, *args):
        return subprocess.run(["bash", str(SCRIPT), *args], cwd=self.root, capture_output=True, text=True, check=False, env=ENV)

    def assert_next_tag(self, want, *args):
        result = self.next_tag(*args)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), want)

    def assert_nothing_releasable(self, *args):
        result = self.next_tag(*args)
        self.assertEqual(result.returncode, NOTHING_RELEASABLE, result.stdout + result.stderr)
        self.assertEqual(result.stdout, "")
        self.assertIn("nothing releasable since v0.22.0", result.stderr)

    def test_feat_bumps_the_minor(self):
        self.commit("fix: a", "feat(cli): b", "docs: c")
        self.assert_next_tag("v0.23.0", "main")

    def test_a_breaking_change_bumps_the_minor(self):
        for subject in ("fix(api)!: drop the v1 field", "feat!: replace the config file", "refactor(agent)!: rename the socket"):
            with self.subTest(subject=subject):
                self.git("reset", "--quiet", "--hard", "v0.22.0")
                self.commit(subject)
                self.assert_next_tag("v0.23.0", "main")

    def test_a_breaking_change_footer_bumps_the_minor(self):
        for footer in ("BREAKING CHANGE: the v1 field is gone.", "BREAKING-CHANGE: the v1 field is gone."):
            for subject in ("refactor(api): remove a public field", "fix(api): drop the v1 field"):
                with self.subTest(footer=footer, subject=subject):
                    self.git("reset", "--quiet", "--hard", "v0.22.0")
                    self.commit(f"{subject}\n\nRemoves it.\n\n{footer}\nSigned-off-by: fixture <fixture@example.com>")
                    self.assert_next_tag("v0.23.0", "main")

    def test_a_footer_before_the_sign_off_paragraph_is_a_breaking_change(self):
        bodies = (
            "Removes it.\n\nBREAKING CHANGE: the v1 field is gone.\n\nSigned-off-by: fixture <fixture@example.com>",
            "* refactor(api): remove a public field\n\nBREAKING CHANGE: the v1 field is gone.\n\n"
            "Signed-off-by: fixture <fixture@example.com>\n\n* docs: say so\n\nSigned-off-by: fixture <fixture@example.com>",
        )
        for body in bodies:
            with self.subTest(body=body):
                self.git("reset", "--quiet", "--hard", "v0.22.0")
                self.commit(f"refactor(api): remove a public field (#1)\n\n{body}")
                self.assert_next_tag("v0.23.0", "main")

    def test_a_footer_in_a_fenced_example_is_not_a_breaking_change(self):
        for fence in ("```", "~~~", "  ```text"):
            with self.subTest(fence=fence):
                self.git("reset", "--quiet", "--hard", "v0.22.0")
                self.commit(f"docs: show the footer\n\nWrite it as:\n\n{fence}\nBREAKING CHANGE: the v1 field is gone.\n{fence.strip()[:3]}\n")
                self.assert_nothing_releasable("main")
                self.commit("fix: a")
                self.assert_next_tag("v0.22.1", "main")

    def test_a_fence_closes_only_on_a_matching_fence(self):
        examples = (
            "````\n```\nBREAKING CHANGE: the v1 field is gone.\n```\n````",
            "```\n~~~\nBREAKING CHANGE: the v1 field is gone.\n```",
            "```\n```text\nBREAKING CHANGE: the v1 field is gone.\n```",
        )
        for example in examples:
            with self.subTest(example=example):
                self.git("reset", "--quiet", "--hard", "v0.22.0")
                self.commit(f"docs: show nested fences\n\n{example}\n")
                self.assert_nothing_releasable("main")

    def test_four_space_indented_code_is_not_a_fence(self):
        self.commit("refactor(api): remove a public field\n\n    ````\n    ```\n\nBREAKING CHANGE: the v1 field is gone.")
        self.assert_next_tag("v0.23.0", "main")

    def test_a_footer_after_a_closed_fence_is_a_breaking_change(self):
        self.commit("refactor(api): remove a public field\n\n```\nan example\n```\n\nBREAKING CHANGE: the v1 field is gone.")
        self.assert_next_tag("v0.23.0", "main")

    def test_an_unclosed_fence_does_not_hide_the_next_commits_footer(self):
        self.commit("refactor(api): remove a public field\n\nBREAKING CHANGE: the v1 field is gone.")
        self.commit("docs: a\n\n```\nnever closed")
        self.assert_next_tag("v0.23.0", "main")

    def test_a_footer_lookalike_is_not_a_breaking_change(self):
        self.commit(
            "docs: a\n\nThis is not a BREAKING CHANGE: it only mentions one.",
            "docs: b\n\nbreaking change: lowercase is not the token.",
            "docs: c\n\nBREAKING CHANGES: the plural is not the token.",
        )
        self.assert_nothing_releasable("main")

    def test_fix_bumps_the_patch(self):
        self.commit("fix: a", "docs: b", "fix(controller): c")
        self.assert_next_tag("v0.22.1", "main")

    def test_docs_ci_and_chore_release_nothing(self):
        self.commit("docs: a", "ci(qualify): b", "chore(deps): c", "test: d", "refactor: e")
        self.assert_nothing_releasable("main")
        self.assert_nothing_releasable("main", "beta")

    def test_a_subject_that_only_starts_like_feat_or_fix_is_neither(self):
        self.commit("feature flags for the agent", "fixup: a", "fixes: b", 'Revert "feat: c"', "Merge branch 'feat/d'")
        self.assert_nothing_releasable("main")

    def test_only_commits_since_the_latest_ga_count(self):
        self.commit("feat: a")
        self.git("tag", "v0.23.0")
        self.commit("fix: b")
        self.assert_next_tag("v0.23.1", "main")

    def test_the_committish_bounds_the_range(self):
        fix = self.commit("fix: a")
        self.commit("feat: b")
        self.assert_next_tag("v0.22.1", fix)
        self.assert_next_tag("v0.23.0", "main")

    def test_beta_numbers_increment(self):
        self.commit("feat: a")
        self.assert_next_tag("v0.23.0-beta.1", "main", "beta")
        for n in range(1, 11):
            self.git("tag", f"v0.23.0-beta.{n}")
        self.git("tag", "v0.22.1-beta.40")
        self.assert_next_tag("v0.23.0-beta.11", "main", "beta")

    def test_rc_numbers_follow_the_betas_version(self):
        self.commit("feat: a")
        self.git("tag", "v0.23.0-beta.4")
        self.assert_next_tag("v0.23.0-rc.1", "main", "rc")
        self.git("tag", "v0.23.0-rc.1")
        self.assert_next_tag("v0.23.0-rc.2", "main", "rc")

    def test_a_commit_the_latest_ga_already_released_has_nothing_releasable(self):
        beta = self.commit("feat: a")
        self.git("tag", "v0.23.0-beta.1")
        self.git("tag", "v0.23.0")
        result = self.next_tag(beta, "rc")
        self.assertEqual(result.returncode, NOTHING_RELEASABLE, result.stdout + result.stderr)
        self.assertIn("nothing releasable since v0.23.0", result.stderr)


if __name__ == "__main__":
    unittest.main()
