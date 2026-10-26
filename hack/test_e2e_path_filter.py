# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the e2e lane path filter against fixtures and the real lane workflows."""

import importlib.util
from pathlib import Path
import shutil
import subprocess
import tempfile
import textwrap
import unittest


SCRIPT = Path(__file__).with_name("e2e-path-filter.py")
ROOT = SCRIPT.parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"
_spec = importlib.util.spec_from_file_location("e2e_path_filter", SCRIPT)
f = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(f)


def decide(workflow, changed):
    with tempfile.TemporaryDirectory() as directory:
        listing = Path(directory) / "changed.txt"
        listing.write_text("\n".join(changed) + "\n", encoding="utf-8")
        out = subprocess.run(
            ["python3", "-B", str(SCRIPT), "decide", "--workflow", str(workflow), "--changed-files", str(listing)],
            capture_output=True, text=True, check=True,
        )
    return out.stdout.strip()


def scratch(test):
    """A temporary directory removed when the test finishes, pass or fail."""
    directory = Path(tempfile.mkdtemp())
    test.addCleanup(shutil.rmtree, directory, ignore_errors=True)
    return directory


class GlobTest(unittest.TestCase):
    def test_github_filter_patterns(self):
        cases = [
            ("controller/**", "controller/internal/server/server.go", True),
            ("controller/**", "agent/main.go", False),
            ("Makefile", "Makefile", True),
            ("Makefile", "controller/Makefile", False),
            ("controller/*.yaml", "controller/hack/x.yaml", False),
            ("controller/*.yaml", "controller/x.yaml", True),
            ("**/*.sql", "migrate.sql", True),
            ("**/*.sql", "a/b/migrate.sql", True),
            ("agent/internal/agent/open_vswitch*.go", "agent/internal/agent/open_vswitch_bridge.go", True),
            (".github/workflows/nico-e2e.yml", ".github/workflowsXnico-e2e.yml", False),
            ("docs/*.mdx?", "docs/page.md", True),
            ("os-image/v[0-9]+.yml", "os-image/v12.yml", True),
            ("os-image/v[0-9]+.yml", "os-image/vx.yml", False),
            ("controller/a\\+b.go", "controller/a+b.go", True),
            ("controller/a\\+b.go", "controller/aab.go", False),
        ]
        for pattern, path, want in cases:
            with self.subTest(pattern=pattern, path=path):
                self.assertEqual(f.matches([pattern], path), want)

    def test_negation_applies_in_order(self):
        patterns = ["os-image/**", "!os-image/**/*_test.go"]
        self.assertTrue(f.matches(patterns, "os-image/checker/main.go"))
        self.assertFalse(f.matches(patterns, "os-image/checker/main_test.go"))
        self.assertTrue(f.matches(patterns + ["os-image/checker/keep_test.go"], "os-image/checker/keep_test.go"))


class LaneInputTest(unittest.TestCase):
    def test_never_a_lane_input(self):
        cases = [
            ("docs/architecture.md", False),
            ("controller/docs/diagram.png", False),
            ("README.md", False),
            ("cli/docs/user-flow.mdx", False),
            ("agent/internal/agent/agent_test.go", False),
            ("agent/internal/agent/testdata/state.json", False),
            ("controller/test/e2e/suite_test.go", True),
            ("controller/test/e2e/testdata/cluster.yaml", True),
            ("cli/e2e/bootstrap/substrate/substrate_test.go", True),
            ("controller/nico-sim/test/integration/testutil/testdata/vm.xml", True),
            ("controller/internal/server/server.go", True),
            (".github/workflows/nico-e2e.yml", True),
        ]
        for path, want in cases:
            with self.subTest(path=path):
                self.assertEqual(f.is_lane_input(path), want)


class DecideTest(unittest.TestCase):
    def fixture(self, paths):
        workflow = scratch(self) / "lane.yml"
        body = "on:\n  push:\n    branches: [main, \"pull-request/[0-9]+\"]\n"
        if paths:
            body += "    paths:\n" + "".join(f"      - {p!r}\n" for p in paths)
        workflow.write_text(body + "jobs: {}\n", encoding="utf-8")
        return workflow

    def test_fixture_decisions(self):
        lane = self.fixture(["controller/**", "agent/**"])
        self.assertEqual(decide(lane, ["controller/docs/runbook.md", "docs/index.yml"]), "false")
        self.assertEqual(decide(lane, ["os-image/packages/cni-ovn/control"]), "false")
        self.assertEqual(decide(lane, ["agent/internal/agent/agent_test.go"]), "false")
        self.assertEqual(decide(lane, ["docs/x.md", "controller/internal/server/server.go"]), "true")
        self.assertEqual(decide(lane, []), "true", "an empty file list runs the lane")
        self.assertEqual(decide(self.fixture([]), ["docs/x.md"]), "true", "a lane without push paths always runs")

    def test_real_lanes(self):
        # (lane, files that must run it, files that must not)
        cases = [
            ("nico-e2e.yml", ["controller/internal/workflows/provision.go"], ["cli/cmd/nvkectl/main.go", "os-image/efi/mkosi.conf"]),
            ("capd-e2e.yml", ["cli/internal/nvkectl/create.go"], ["cli/internal/bootstrap/plan.go", "controller/docs/capd.md"]),
            ("capd-dsx-identity-exchange-e2e.yml", ["agent/cmd/agent/main.go"], ["controller/docs/identity.md", "management-engine/api/types.go"]),
            ("nico-vms-e2e.yml", ["controller/test/e2e/nico_vms_test.go"], ["controller/internal/server/server.go"]),
            ("local-entrypoint-e2e.yml", ["controller/Tiltfile"], ["controller/internal/server/server.go"]),
            ("os-image-artifact.yml", ["os-image/packer/ubuntu.pkr.hcl"], ["os-image/checker/check_test.go", "controller/main.go"]),
            ("os-image-efi.yml", ["os-image/efi/mkosi.conf"], ["os-image/packer/ubuntu.pkr.hcl"]),
            ("nvkectl-bootstrap-e2e.yml", ["cli/internal/bootstrap/plan.go"], ["cli/docs/sitespec.md", "os-image/packer/ubuntu.pkr.hcl"]),
            ("kubeadm-bringup-e2e.yml", ["controller/test/kubeadm/scripts/setup-ci-host.sh"], ["controller/internal/server/server.go"]),
            ("nktp-e2e.yml", ["nktp/deploy/charts/nktp/values.yaml"], ["controller/main.go"]),
        ]
        for lane, run, skip in cases:
            with self.subTest(lane=lane):
                self.assertEqual(decide(WORKFLOWS / lane, run), "true")
                self.assertEqual(decide(WORKFLOWS / lane, skip), "false")
                self.assertEqual(decide(WORKFLOWS / lane, ["docs/index.yml", "README.md"]), "false", "docs-only")

    def test_filter_script_runs_every_lane(self):
        for lane in sorted(p.name for p in WORKFLOWS.glob("*.yml") if "e2e-pr-path-filter.yml" in p.read_text(encoding="utf-8")):
            if lane == "e2e-pr-path-filter.yml":
                continue
            with self.subTest(lane=lane):
                self.assertEqual(decide(WORKFLOWS / lane, ["hack/e2e-path-filter.py"]), "true")

    def test_shared_kind_action_runs_every_lane_that_uses_it(self):
        lanes = ["nico-e2e.yml", "nico-vms-e2e.yml", "capd-e2e.yml", "capd-dsx-identity-exchange-e2e.yml",
                 "local-entrypoint-e2e.yml", "nvkectl-bootstrap-e2e.yml", "nktp-e2e.yml"]
        for lane in lanes:
            with self.subTest(lane=lane):
                self.assertEqual(decide(WORKFLOWS / lane, [".github/actions/setup-kind/action.yml"]), "true")


class CheckTest(unittest.TestCase):
    LANE = textwrap.dedent("""\
        on:
          push:
            branches: [main, "pull-request/[0-9]+"]
            paths: {paths}
        jobs:
        {jobs}""")
    FILTER = "  changes:\n    uses: ./.github/workflows/e2e-pr-path-filter.yml\n"
    GATED = "  e2e:\n    needs: changes\n    if: needs.changes.outputs.run_e2e == 'true'\n    runs-on: arc-arm64-cpu16\n"
    PATHS = ["controller/**", ".github/workflows/e2e-pr-path-filter.yml", "hack/e2e-path-filter.py"]

    def run_check(self, jobs, paths=None, files=None):
        """Check a fixture repository holding .github/workflows/lane.yml and files."""
        root = scratch(self)
        workflows = root / ".github" / "workflows"
        workflows.mkdir(parents=True)
        (workflows / "lane.yml").write_text(self.LANE.format(paths=paths or self.PATHS, jobs=jobs), encoding="utf-8")
        for name, content in (files or {}).items():
            (root / name).parent.mkdir(parents=True, exist_ok=True)
            (root / name).write_text(content, encoding="utf-8")
        return subprocess.run(["python3", "-B", str(SCRIPT), "check", "--workflows", str(workflows)], capture_output=True, text=True)

    def test_repository_passes(self):
        out = subprocess.run(["python3", "-B", str(SCRIPT), "check", "--workflows", str(WORKFLOWS)], capture_output=True, text=True)
        self.assertEqual(out.returncode, 0, out.stderr)

    def test_unfiltered_self_hosted_lane_fails(self):
        out = self.run_check("  e2e:\n    runs-on: arc-arm64-cpu16\n")
        self.assertEqual(out.returncode, 1)
        self.assertIn("no job uses", out.stderr)

    def test_job_that_skips_the_gate_fails(self):
        out = self.run_check(self.FILTER + "  e2e:\n    needs: changes\n    runs-on: linux-amd64-cpu16\n")
        self.assertEqual(out.returncode, 1)
        self.assertIn("does not test needs.changes.outputs.run_e2e", out.stderr)
        out = self.run_check(self.FILTER + "  e2e:\n    runs-on: linux-amd64-cpu16\n")
        self.assertIn("does not need changes", out.stderr)

    def test_gated_lane_passes(self):
        jobs = self.FILTER + (
            "  e2e:\n    needs: changes\n    if: needs.changes.outputs.run_e2e == 'true'\n    runs-on: arc-arm64-cpu16\n"
            "  publish:\n    needs: e2e\n    runs-on: ubuntu-latest\n"
        )
        self.assertEqual(self.run_check(jobs).returncode, 0)

    def test_bare_or_overridden_conditions_fail(self):
        out = self.run_check(self.FILTER + "  e2e:\n    needs: changes\n    if: needs.changes.outputs.run_e2e\n    runs-on: arc-arm64-cpu16\n")
        self.assertEqual(out.returncode, 1, "a bare output is a non-empty string, so 'false' is truthy")
        out = self.run_check(self.FILTER + (
            "  prepare:\n    needs: changes\n    if: needs.changes.outputs.run_e2e == 'true'\n    runs-on: ubuntu-latest\n"
            "  e2e:\n    needs: prepare\n    if: always()\n    runs-on: arc-arm64-cpu16\n"))
        self.assertEqual(out.returncode, 1)
        self.assertIn("can run after a skipped job", out.stderr)
        out = self.run_check(self.FILTER + "  e2e:\n    needs: changes\n    if: always() || needs.changes.outputs.run_e2e == 'true'\n    runs-on: arc-arm64-cpu16\n")
        self.assertEqual(out.returncode, 1, "an override beside the gate test still runs the job")

    def test_runner_forms_are_detected(self):
        forms = [
            "  e2e:\n    runs-on: [self-hosted, linux-amd64-cpu16]\n",
            "  e2e:\n    runs-on: [self-hosted, Linux, X64]\n",
            "  e2e:\n    strategy:\n      matrix:\n        runner: [ubuntu-latest, arc-arm64-cpu16]\n    runs-on: ${{ matrix.runner }}\n",
            "  e2e:\n    uses: ./.github/workflows/backend-e2e.yml\n    with:\n      runner: linux-amd64-cpu16\n",
        ]
        for jobs in forms:
            with self.subTest(jobs=jobs):
                self.assertEqual(self.run_check(jobs).returncode, 1)

    def test_unlisted_local_action_fails(self):
        action = {".github/actions/setup-kind/action.yml": "runs:\n  using: composite\n  steps: []\n"}
        jobs = self.FILTER + self.GATED + "    steps:\n      - uses: ./.github/actions/setup-kind\n"
        out = self.run_check(jobs, files=action)
        self.assertEqual(out.returncode, 1)
        self.assertIn("uses ./.github/actions/setup-kind, but its push paths do not list .github/actions/setup-kind/action.yml", out.stderr)
        self.assertEqual(self.run_check(jobs, paths=self.PATHS + [".github/actions/setup-kind/**"], files=action).returncode, 0)

    def test_unlisted_action_behind_a_called_workflow_fails(self):
        files = {
            ".github/workflows/body.yml": "on: workflow_call\njobs:\n  run:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/setup-kind\n",
            ".github/actions/setup-kind/action.yml": "runs:\n  using: composite\n  steps: []\n",
        }
        jobs = self.FILTER + "  e2e:\n    needs: changes\n    if: needs.changes.outputs.run_e2e == 'true'\n    uses: ./.github/workflows/body.yml\n    with:\n      runner: arc-arm64-cpu16\n"
        out = self.run_check(jobs, paths=self.PATHS + [".github/workflows/body.yml"], files=files)
        self.assertEqual(out.returncode, 1)
        self.assertIn("uses ./.github/actions/setup-kind", out.stderr)
        self.assertNotIn("body.yml", out.stderr)

    def test_references_are_read_from_parsed_yaml(self):
        action = {".github/actions/setup-kind/action.yml": "runs:\n  using: composite\n  steps: []\n"}
        for step in ('      - "uses": ./.github/actions/setup-kind\n', "      - {uses: ./.github/actions/setup-kind}\n"):
            with self.subTest(step=step):
                out = self.run_check(self.FILTER + self.GATED + "    steps:\n" + step, files=action)
                self.assertIn("uses ./.github/actions/setup-kind", out.stderr)
        script = self.FILTER + self.GATED + "    steps:\n      - run: |\n          echo 'uses: ./.github/actions/setup-kind'\n"
        self.assertEqual(self.run_check(script, files=action).returncode, 0, "text in a run: script is not a reference")

    def test_every_file_of_an_action_must_be_listed(self):
        files = {
            ".github/actions/tool/action.yml": "runs:\n  using: node24\n  main: index.js\n",
            ".github/actions/tool/index.js": "console.log(1)\n",
        }
        jobs = self.FILTER + self.GATED + "    steps:\n      - uses: ./.github/actions/tool\n"
        out = self.run_check(jobs, paths=self.PATHS + [".github/actions/tool/action.yml"], files=files)
        self.assertIn("uses ./.github/actions/tool", out.stderr)
        self.assertEqual(self.run_check(jobs, paths=self.PATHS + [".github/actions/tool/**"], files=files).returncode, 0)

    def test_unlisted_filter_script_fails(self):
        jobs = self.FILTER + self.GATED
        out = self.run_check(jobs, paths=["controller/**", ".github/workflows/e2e-pr-path-filter.yml"])
        self.assertEqual(out.returncode, 1)
        self.assertIn("do not list hack/e2e-path-filter.py", out.stderr)

    def test_second_filter_job_fails(self):
        jobs = self.FILTER + "  changes-again:\n    uses: ./.github/workflows/e2e-pr-path-filter.yml\n" + self.GATED
        out = self.run_check(jobs)
        self.assertEqual(out.returncode, 1)
        self.assertIn("one filter job per lane is supported", out.stderr)

    def test_github_hosted_lane_is_exempt(self):
        self.assertEqual(self.run_check("  e2e:\n    runs-on: ubuntu-latest\n").returncode, 0)


if __name__ == "__main__":
    unittest.main()
