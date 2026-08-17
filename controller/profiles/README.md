# External profiles

Fleet OS and config profiles no longer ship in this repository or the CWE
container image. An external profile source packages and mounts them at runtime:

- Profile content is mounted at `PROFILES_DIR`.
- Profile-hook scripts are mounted at `RUNBOOKS_DIR`.
- The configured OCI content image provides both directories as one versioned
  runtime bundle.

For local Tilt, point `CWE_PROFILES_DIR` at the external bundle's `profiles/`
directory (see Tiltfile). Tilt builds `docker.local/cwe-profiles:tilt` from the
Dockerfile and `runbooks/` directory alongside it.
Unit tests and gofail e2e use `test/fixtures/profiles` (including
`osProfiles/metadata.yaml` for DB migrations that load profile metadata).
Set `CWE_PROFILES_DIR` to exercise the full external profile bundle.
