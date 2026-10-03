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
`<output_directory>/<non-fde|fde>-apt-acquisitions.tar`. The QCOW2 filename and
the existing source-owned final conversion are unchanged. The capture hook,
package copies and temporary recorder/archive are removed from the target before
final image cleanup. The caller must collect and authenticate this extra output;
writing a file does not establish a Run or QCOW2 evidence binding.

The capture's `nkd.dev/package-acquisitions/v1` document contains architecture,
scope `apt-pre-install-pkgs` and a digest-sorted package list. Each package records
name, version, architecture, digest, size, digest-derived member path and sorted
repository metadata URIs. Repository URIs are available locations from matching
APT metadata, not actual HTTP redirect observations or repository-signature proof.

Limits are 16,384 captured files, 1 GiB per file and 7 GiB captured bytes. Capture
consumes VM workspace storage; insufficient storage or unavailable exact package
metadata fails the build. Existing archive output is never replaced by the source
recorder. No package URL is guessed when repository metadata has disappeared.

This is not complete OS inventory or a full disk SBOM. Base ISO content,
non-APT downloads and package events after the export window are not covered.
The hook's installer-to-target behavior still requires a real NKE image build;
native APT container tests and Packer validation alone do not prove that handoff.
Captured archives whose exact repository metadata has disappeared, or whose
matching locations use `file:` or `cdrom:` instead of HTTP(S), fail export. The
current installer-media path has not been exercised with capture enabled.

Run the ordinary source contract suite with `make -C os-image/packer test`.
Set `NKE_TEST_PACKER` to a preinstalled Packer executable and optionally
`NKE_TEST_APT_CONFIG` to native apt-config to run the Linux tool validations in
`test_capture_apt_acquisitions.py`. The runner supplies its pinned plugin closure;
these tests do not run `packer init` or start QEMU.
