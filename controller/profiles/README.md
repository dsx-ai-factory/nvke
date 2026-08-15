# Profiles moved (NKX-13910)

Fleet OS and config profiles no longer ship in this repository or the CWE
container image. They live in **nke-site-deploy** and are mounted at runtime:

- Editable tree: `nke-site-deploy/cwe-profiles/profiles/`
- Delivery: OCI image `nvcr.io/j7sbcjl3qgta/cwe-profiles` (initContainer `cp`
  into `PROFILES_DIR`)

For local Tilt, point `CWE_PROFILES_DIR` at that tree (see Tiltfile); Tilt
builds `docker.local/cwe-profiles:tilt` from the sibling Dockerfile.
Unit tests and gofail e2e use `test/fixtures/profiles` (including
`osProfiles/metadata.yaml` for DB migrations that load profile metadata).
Optionally use a sibling site-deploy checkout for fuller profile coverage.
