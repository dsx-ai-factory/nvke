#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Offline regression: source lookup results must not alter attribution.
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
mkdir -p "${work_dir}/bin"
cat > "${work_dir}/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == 'list -m' ]]; then
    echo 'github.com/example/agent'
else
    cat "${NOTICE_TEST_MODULES}"
fi
GO
cat > "${work_dir}/bin/go-licenses" <<'LICENSES'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == csv ]]; then
    cat "${NOTICE_TEST_ROWS}"
    exit
fi
for arg in "$@"; do
    if [[ "${arg}" == --save_path=* ]]; then
        while IFS=, read -r pkg _; do
            dir="${arg#--save_path=}/${pkg}"
            mkdir -p "${dir}"
            if [[ "${NOTICE_TEST_SAVE:-all}" != all ]]; then
                printf 'package library\n' > "${dir}/library.go"
                continue
            fi
            printf 'Required license fixture\n' > "${dir}/LICENSE"
            printf 'Required attribution fixture\n' > "${dir}/NOTICE"
        done < "${NOTICE_TEST_ROWS}"
    fi
done
LICENSES
chmod 700 "${work_dir}/bin/go" "${work_dir}/bin/go-licenses"

# generate <output> <rows-file> <modules-file>: run the generator against fixtures.
generate() {
    PATH="${work_dir}/bin:${PATH}" GO_LICENSES="${work_dir}/bin/go-licenses" \
        NOTICE_TEST_ROWS="$2" NOTICE_TEST_MODULES="$3" OUTPUT="$1" \
        bash "${script_dir}/generate-notices.sh"
}

modules="${work_dir}/modules"
echo 'github.com/example/library/subpackage github.com/example/library@v1.2.3' > "${modules}"

# Source URL availability must not change the output.
for discovery in available unavailable; do
    source_url='https://github.com/example/library/blob/v1.2.3/LICENSE'
    [[ "${discovery}" == available ]] || source_url=Unknown
    printf 'github.com/example/library,%s,Apache-2.0\n' "${source_url}" > "${work_dir}/${discovery}.csv"
    generate "${work_dir}/${discovery}.md" "${work_dir}/${discovery}.csv" "${modules}"
done
cmp "${work_dir}/available.md" "${work_dir}/unavailable.md"
grep -q -F 'Required license fixture' "${work_dir}/unavailable.md"
grep -q -F 'Required attribution fixture' "${work_dir}/unavailable.md"
grep -q -F 'https://pkg.go.dev/github.com/example/library@v1.2.3' "${work_dir}/unavailable.md"
if grep -q -F '(Unknown)' "${work_dir}/unavailable.md"; then
    echo 'ERROR: generated a broken Unknown link' >&2
    exit 1
fi

# Major-version source URL variants, and a committed file that holds a different
# variant, must not change the output.
echo 'github.com/example/v5 github.com/example/v5@v5.0.0' > "${work_dir}/modules-v5"
for variant in with without; do
    path=v5/; [[ "${variant}" == with ]] || path=
    printf 'github.com/example/v5,https://github.com/example/v5/blob/v5.0.0/%sLICENSE,MIT\n' "${path}" > "${work_dir}/${variant}.csv"
done
generate "${work_dir}/fresh.md" "${work_dir}/with.csv" "${work_dir}/modules-v5"
cp "${work_dir}/fresh.md" "${work_dir}/committed.md"
generate "${work_dir}/without.md" "${work_dir}/without.csv" "${work_dir}/modules-v5"
generate "${work_dir}/committed.md" "${work_dir}/without.csv" "${work_dir}/modules-v5"
cmp "${work_dir}/fresh.md" "${work_dir}/without.md"
cmp "${work_dir}/fresh.md" "${work_dir}/committed.md"

# Dual-license labels are ordered canonically, whatever URL each row carries.
printf 'github.com/example/library,https://example.test/a,MIT\ngithub.com/example/library,https://example.test/b,Apache-2.0\n' > "${work_dir}/dual-a.csv"
printf 'github.com/example/library,https://example.test/a,Apache-2.0\ngithub.com/example/library,https://example.test/b,MIT\n' > "${work_dir}/dual-b.csv"
generate "${work_dir}/dual-a.md" "${work_dir}/dual-a.csv" "${modules}"
generate "${work_dir}/dual-b.md" "${work_dir}/dual-b.csv" "${modules}"
cmp "${work_dir}/dual-a.md" "${work_dir}/dual-b.md"
grep -q -F '| Apache-2.0 / MIT |' "${work_dir}/dual-a.md"

# A third-party package carrying this repository's URL is rejected, including
# when the URL has a major-version segment.
printf 'github.com/example/library,https://github.com/example/agent/blob/v1.0.0/v1/LICENSE,MIT\n' > "${work_dir}/self.csv"
if generate "${work_dir}/self.md" "${work_dir}/self.csv" "${modules}" 2> "${work_dir}/self.err"; then
    echo 'ERROR: accepted a third-party package attributed to this repository' >&2
    exit 1
fi
grep -q -F 'are not ours' "${work_dir}/self.err"

# A package with no saved attribution file is rejected.
if NOTICE_TEST_SAVE=none generate "${work_dir}/missing.md" "${work_dir}/available.csv" "${modules}" 2> "${work_dir}/missing.err"; then
    echo 'ERROR: accepted a package with no attribution file' >&2
    exit 1
fi
grep -q -F 'no attribution file for github.com/example/library' "${work_dir}/missing.err" || {
    cat "${work_dir}/missing.err" >&2
    echo 'ERROR: expected the missing-attribution diagnostic' >&2
    exit 1
}
echo 'Notice source availability and complete attribution checks passed.'
