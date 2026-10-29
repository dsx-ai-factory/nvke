# Optional APT archive capture

Set `capture_apt_acquisitions=true` when a build runner needs the actual Debian
archives consumed by APT before source cleanup. The default is false. Both
architectures and both image variants use the same capture contract.

The APT pre-install hook retains archive bytes, including repeated filenames.
Before final provisioning clears APT indexes and the build SSH key, the recorder
checks each archive with dpkg and matches its version, architecture, SHA-256 and
size against python3-apt repository metadata. It exports an uncompressed tar,
with canonical `manifest.json` first and digest-named package members after it.

Packer downloads that separate file to
`<output_directory>/<non-fde|fde>/apt-acquisitions.tar`. The QCOW2 filename and
the existing source-owned final conversion are unchanged. The capture hook,
package copies and temporary recorder/archive are removed from the target before
final image cleanup. The caller must collect and authenticate this extra output;
writing a file does not establish a Run or QCOW2 evidence binding.
The tar is valid only when `packer build` exits successfully. A failed build
can leave incomplete output; Packer's builder output lifecycle owns the variant
directory. The Make targets do not collect the capture; use direct Packer.

The capture's `nkd.dev/package-acquisitions/v1` document contains architecture,
scope `apt-pre-install-pkgs` and a digest-sorted package list. Each package records
name, version, architecture, digest, size, digest-derived member path and sorted
repository metadata URIs. Repository URIs are available locations from matching
APT metadata, not actual HTTP redirect observations or repository-signature proof.
Those URIs follow the installer's elected mirror and may include several indexes
carrying the same version. Manifest bytes are deterministic only for the same
captured bytes and metadata, including the elected mirror. A URI is a descriptive
candidate location; a consumer must verify bytes fetched from it against the
recorded digest.

Limits are 16,384 captured files, 1 GiB per file and 7 GiB captured bytes. Capture
consumes VM workspace storage; insufficient storage or unavailable exact package
metadata fails the build. Budget at least twice the captured bytes for the hook's
copies and export tar, plus the installed OS, staged image inputs and any APT
cache still retained. The 7 GiB recorder limit is not a reservation and can exceed
the default 12G build disk; increase `disk_size` for larger closures. A hook
failure such as ENOSPC aborts installation and is diagnosed through the installer
serial log, even when Packer reports an SSH timeout. The guest recorder never
replaces an existing guest archive output.

Ubuntu updates/security indexes can supersede a package version after its
archive was captured and before export reads the refreshed indexes. Missing
exact metadata fails export without guessing a URL. This is a known retryable
build failure; rerun against current indexes. Capturing an index snapshot at
acquisition time is not implemented.

This is not complete OS inventory or a full disk SBOM. Base ISO content,
non-APT downloads and package events after the export window are not covered.
That includes Open vSwitch: `provision.sh` installs DOCA's
`doca-openvswitch-switch` after the export, so neither the DOCA debs (pinned by
`packages/doca-ovs/lock/` instead) nor the Ubuntu archive packages they pull in
are in the capture.
The hook's installer-to-target behavior still requires a real NVKE image build;
native APT container tests and Packer validation alone do not prove that handoff.
Captured archives whose exact repository metadata has disappeared, or whose
matching locations include any `file:` or `cdrom:` URI, fail export, even when
an HTTP(S) location is also available. The current installer-media path has not
been exercised with capture enabled.

Run the ordinary source contract suite with `make -C os-image/packer test`.
Optional native tools exercise additional cases in
`test_capture_apt_acquisitions.py`:

- `NKE_TEST_APT_CONFIG=apt-config` validates the hook with native APT and checks
  retention of repeated archive filenames; CI sets it.
  The real Debian metadata test runs whenever `dpkg-deb` is available.
- `NKE_TEST_PACKER=/path/to/packer` renders and decodes both autoinstall variants
  with capture enabled and disabled. This template test needs no plugins and
  does not start QEMU. The advisory OS Image Artifact `validate-packer` job runs
  it after installing pinned Packer.

Also run `make -C os-image/packer validate-packer` as a required local developer
check: the maintained script owns plugin initialization and the matrix of both
architectures, variants and capture settings. The advisory OS Image Artifact
`validate-packer` job runs the same target. The required merge gate runs the
source contract tests without Packer; it does not run this HCL matrix or YAML
rendering. The unit test suite does not duplicate the matrix fixtures.
