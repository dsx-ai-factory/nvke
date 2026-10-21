#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Inventory, and on --apply delete, the binaries and container images this
# repository has pushed to GitHub: release assets, release records, GHCR
# packages, Actions artifacts and Actions caches. Git tags and source are never
# touched. Dry run by default; every run writes its inventory and plan to --out.

set -euo pipefail

usage() {
	cat <<'EOF'
usage: hack/github-artifacts-cleanup.sh [scope...] [filter...] [--apply [--yes]] [options]

Scopes (at least one, or --all):
  --release-assets    delete every release asset, keep the release records
  --releases          delete the release records too (drafts included); tags stay;
                      a release with an asset the filters exclude is kept as well
  --packages          delete the GHCR packages this repository published
  --artifacts         delete the Actions artifacts older than --older-than days
  --caches            delete the Actions caches not used for --older-than days
  --all               every scope above

Filters (release tags and asset names, every tag of a package version,
artifact names and cache keys; --keep wins over --only):
  --only REGEX        touch only items with a name matching (bash regex)
  --keep REGEX        never touch items with a name matching
  --before DATE       touch only items created before DATE (YYYY-MM-DD)
  --older-than DAYS   age floor for artifacts and caches (default 2)
  --untagged          with a filter, also delete untagged package versions
                      (--before applies to them, --only cannot; do not combine
                      with --keep: the untagged versions are the per-arch
                      manifests the kept indexes point at)

Options:
  --repo OWNER/REPO   repository (default: the checkout's GitHub remote;
                      required with --yes)
  --package-prefix P  also select GHCR packages named P/... (repeatable; the
                      repository's own name and every package linked to the
                      repository are selected without this)
  --package-match RE  narrow the selected packages to names matching RE
  --apply             delete; without it the script only reports
  --yes               skip the confirmation prompt (needs --apply and --repo)
  --out DIR           where the TSV inventory, plan and results go

GitHub rules this script works around: a package's last version cannot be
deleted on its own (the package is deleted instead when a filter selects
every version); a public package with more than 5,000 downloads cannot be
deleted through the API and is reported as failed. Deleted packages can be
restored for 30 days from the package settings page; deleted releases and
assets cannot. Deleting packages needs a token with delete:packages.
EOF
}

repo=""
out=""
apply=false
yes=false
only=""
keep=""
before=""
older_than=2
untagged=false
pkg_match=""
scope_assets=false
scope_releases=false
scope_packages=false
scope_artifacts=false
scope_caches=false
declare -a prefixes=()

need_value() {
	[ $# -ge 2 ] || { echo "$1 needs a value" >&2; exit 2; }
}
while [ $# -gt 0 ]; do
	case "$1" in
	--release-assets) scope_assets=true ;;
	--releases) scope_releases=true; scope_assets=true ;;
	--packages) scope_packages=true ;;
	--artifacts) scope_artifacts=true ;;
	--caches) scope_caches=true ;;
	--all) scope_assets=true; scope_releases=true; scope_packages=true; scope_artifacts=true; scope_caches=true ;;
	--only) need_value "$@"; only="$2"; shift ;;
	--keep) need_value "$@"; keep="$2"; shift ;;
	--before) need_value "$@"; before="$2"; shift ;;
	--older-than) need_value "$@"; older_than="$2"; shift ;;
	--untagged) untagged=true ;;
	--repo) need_value "$@"; repo="$2"; shift ;;
	--package-prefix) need_value "$@"; prefixes+=("$2"); shift ;;
	--package-match) need_value "$@"; pkg_match="$2"; shift ;;
	--apply) apply=true ;;
	--yes) yes=true ;;
	--out) need_value "$@"; out="$2"; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
	shift
done

if ! $scope_assets && ! $scope_packages && ! $scope_artifacts && ! $scope_caches; then
	echo "pick at least one scope (or --all)" >&2
	usage >&2
	exit 2
fi
if [ -n "$before" ] && ! [[ "$before" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
	echo "--before wants YYYY-MM-DD, got $before" >&2
	exit 2
fi
if ! [[ "$older_than" =~ ^[0-9]+$ ]]; then
	echo "--older-than wants a number of days, got $older_than" >&2
	exit 2
fi
if $yes && [ -z "$repo" ]; then
	echo "--yes needs an explicit --repo" >&2
	exit 2
fi
if $untagged && [ -n "$keep" ]; then
	echo "--untagged cannot be combined with --keep (see --help)" >&2
	exit 2
fi
for tool in gh jq; do
	command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }
done
# gh canonicalizes the name and case; a wrong or misspelled repository fails here.
repo="$(gh repo view "${repo:-}" --json nameWithOwner --jq .nameWithOwner)"
owner="${repo%%/*}"
name="${repo#*/}"
owner_type="$(gh api "users/$owner" --jq '.type')"
case "$owner_type" in
Organization) pkg_base="orgs/$owner" ;;
User) pkg_base="users/$owner" ;;
*) echo "cannot tell whether $owner is an organization or a user" >&2; exit 1 ;;
esac
prefixes+=("$name")
out="${out:-${TMPDIR:-/tmp}/github-cleanup-$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p "$out"
plan="$out/plan.tsv"
: >"$plan"
# Artifacts and caches younger than this stay: the next run may still need them.
actions_cutoff="$(date -u -d "${older_than} days ago" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-"${older_than}d" +%Y-%m-%dT%H:%M:%SZ)"

# Sleep until the core rate limit resets when fewer than 200 requests are left.
# The budget is 5,000 an hour for the whole account, shared with other work.
throttle() {
	local remaining reset now
	remaining="$(gh api rate_limit --jq '.resources.core.remaining')"
	[ "$remaining" -lt 200 ] || return 0
	reset="$(gh api rate_limit --jq '.resources.core.reset')"
	now="$(date -u +%s)"
	echo "rate limit nearly spent ($remaining left); sleeping $((reset - now + 5)) s" >&2
	sleep $((reset - now + 5))
}

# gh api with three attempts on a 5xx or a rate limit; other errors fail at once.
# A secondary rate limit asks for at least a minute before the next request.
api() {
	local attempt
	for attempt in 1 2 3; do
		if gh api "$@" 2>"$out/.api.err"; then
			return 0
		fi
		if grep -q -E 'HTTP 429|rate limit' "$out/.api.err"; then
			sleep 60
			throttle
		elif ! grep -q -E 'HTTP 5[0-9][0-9]' "$out/.api.err"; then
			cat "$out/.api.err" >&2
			return 1
		fi
		sleep $((attempt * 5))
	done
	cat "$out/.api.err" >&2
	return 1
}

# paged ENDPOINT ITEMS FILTER: list ENDPOINT a page at a time and print FILTER
# for every element of the ITEMS array. One failed page must not lose the whole
# listing, which gh --paginate would.
paged() {
	local endpoint="$1" items="$2" filter="$3" page=1 sep='?' n
	[[ "$endpoint" == *\?* ]] && sep='&'
	while :; do
		api "${endpoint}${sep}per_page=100&page=${page}" >"$out/.page.json"
		n=$(jq "${items} | length" "$out/.page.json")
		[ "$n" -gt 0 ] || break
		jq -r "${items}[] | ${filter}" "$out/.page.json"
		[ "$n" -eq 100 ] || break
		page=$((page + 1))
	done
}

# wanted CREATED NAME...: --before first; then --keep on any name excludes;
# then --only must match one name. With no names only the date applies.
wanted() {
	local created="$1" name matched=false
	shift
	if [ -n "$before" ] && ! [[ "${created:0:10}" < "$before" ]]; then return 1; fi
	[ $# -gt 0 ] || return 0
	for name in "$@"; do
		if [ -n "$keep" ] && [[ "$name" =~ $keep ]]; then return 1; fi
		if [ -z "$only" ] || [[ "$name" =~ $only ]]; then matched=true; fi
	done
	$matched
}

filtered() {
	[ -n "$only" ] || [ -n "$keep" ] || [ -n "$before" ]
}

# plan KIND ID LABEL BYTES DELETE_PATH
add() {
	printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$5" >>"$plan"
}

inventory_releases() {
	paged "repos/$repo/releases" . '[.id, (.tag_name // "-"), (if .draft then "draft" else "published" end), (.published_at // .created_at), (.assets | length), ([.assets[].size] | add // 0)] | @tsv' >"$out/releases.tsv"
	paged "repos/$repo/releases" . '.id as $r | (.tag_name // "-") as $t | .assets[] | [$r, $t, .id, (.name // "-"), .size, .created_at] | @tsv' >"$out/release-assets.tsv"
	local rid tag state created count aid aname asize acreated planned
	: >"$out/.planned-releases"
	while IFS=$'\t' read -r rid tag aid aname asize acreated; do
		if wanted "$acreated" "$tag" "$aname"; then
			add asset "$aid" "$tag/$aname" "$asize" "repos/$repo/releases/assets/$aid"
			echo "$rid" >>"$out/.planned-releases"
		fi
	done <"$out/release-assets.tsv"
	if $scope_releases; then
		# Deleting a record deletes its assets too, so a release with an asset
		# the filters excluded stays. The date is the publication date.
		while IFS=$'\t' read -r rid tag state created count _; do
			wanted "$created" "$tag" || continue
			planned=$(grep -c -x "$rid" "$out/.planned-releases" || true)
			if [ "$planned" -ne "$count" ]; then
				echo "keeping release $tag: $((count - planned)) of $count assets are excluded by the filters"
				continue
			fi
			add release "$rid" "$tag ($state)" 0 "repos/$repo/releases/$rid"
		done <"$out/releases.tsv"
	fi
}

inventory_packages() {
	paged "$pkg_base/packages?package_type=container" . '[.name, .visibility, (.repository.name // "-"), .created_at, .updated_at] | @tsv' >"$out/packages-all.tsv"
	: >"$out/packages.tsv"
	: >"$out/package-versions.tsv"
	local pname vis prepo pcreated pupdated prefix selected enc
	while IFS=$'\t' read -r pname vis prepo pcreated pupdated; do
		selected=false
		[ "$prepo" = "$name" ] && selected=true
		for prefix in "${prefixes[@]}"; do
			[[ "$pname" == "$prefix/"* ]] && selected=true
		done
		$selected || continue
		if [ -n "$pkg_match" ] && ! [[ "$pname" =~ $pkg_match ]]; then continue; fi
		printf '%s\t%s\t%s\t%s\t%s\n' "$pname" "$vis" "$prepo" "$pcreated" "$pupdated" >>"$out/packages.tsv"
		enc="${pname//\//%2F}"
		paged "$pkg_base/packages/container/$enc/versions" . '[.id, .created_at, ((.metadata.container.tags // []) | join(","))] | @tsv' |
			awk -F'\t' -v n="$pname" 'BEGIN { OFS = "\t" } { print n, $1, $2, $3 }' >>"$out/package-versions.tsv"
	done <"$out/packages-all.tsv"
	local vid vcreated vtags total
	declare -a tags
	while IFS=$'\t' read -r pname vis prepo pcreated pupdated; do
		enc="${pname//\//%2F}"
		total=$(awk -F'\t' -v n="$pname" '$1 == n' "$out/package-versions.tsv" | wc -l | tr -d ' ')
		if ! filtered; then
			add package "$pname" "$pname ($total versions)" 0 "$pkg_base/packages/container/$enc"
			continue
		fi
		# Per version. Each tag is tested on its own, so anchored patterns work
		# on a version that carries both a release tag and a sha- tag. Untagged
		# versions are the per-arch manifests behind the indexes.
		: >"$out/.planned-versions"
		while IFS=$'\t' read -r _ vid vcreated vtags; do
			if [ -z "$vtags" ]; then
				$untagged || continue
				wanted "$vcreated" || continue
			else
				IFS=, read -ra tags <<<"$vtags"
				wanted "$vcreated" "${tags[@]}" || continue
			fi
			printf '%s\t%s\t%s\n' "$vid" "${vtags:-untagged}" "$vcreated" >>"$out/.planned-versions"
		done < <(awk -F'\t' -v n="$pname" '$1 == n' "$out/package-versions.tsv")
		# GitHub refuses to delete a package's last version; a filter that
		# selects every version deletes the package instead.
		if [ "$(wc -l <"$out/.planned-versions" | tr -d ' ')" -eq "$total" ] && [ "$total" -gt 0 ]; then
			add package "$pname" "$pname ($total versions, all selected)" 0 "$pkg_base/packages/container/$enc"
			continue
		fi
		while IFS=$'\t' read -r vid vtags vcreated; do
			add version "$vid" "$pname:$vtags" 0 "$pkg_base/packages/container/$enc/versions/$vid"
		done <"$out/.planned-versions"
	done <"$out/packages.tsv"
}

inventory_artifacts() {
	paged "repos/$repo/actions/artifacts" .artifacts '[.id, (.name // "-"), .size_in_bytes, .expired, .created_at, .expires_at] | @tsv' >"$out/artifacts.tsv"
	local aid aname asize expired created
	while IFS=$'\t' read -r aid aname asize expired created _; do
		[ "$expired" = "true" ] && continue
		[[ "$created" < "$actions_cutoff" ]] || continue
		wanted "$created" "$aname" || continue
		add artifact "$aid" "$aname" "$asize" "repos/$repo/actions/artifacts/$aid"
	done <"$out/artifacts.tsv"
}

inventory_caches() {
	paged "repos/$repo/actions/caches" .actions_caches '[.id, (.key // "-"), (.ref // "-"), .size_in_bytes, .created_at, .last_accessed_at] | @tsv' >"$out/caches.tsv"
	local cid key ref csize created accessed
	while IFS=$'\t' read -r cid key ref csize created accessed; do
		[[ "$accessed" < "$actions_cutoff" ]] || continue
		wanted "$created" "$key" || continue
		add cache "$cid" "$key ($ref)" "$csize" "repos/$repo/actions/caches/$cid"
	done <"$out/caches.tsv"
}

echo "repository $repo ($owner_type), output in $out"
$scope_assets && inventory_releases
$scope_packages && inventory_packages
$scope_artifacts && inventory_artifacts
$scope_caches && inventory_caches

# Listings taken while CI keeps adding items can repeat a row across pages.
awk -F'\t' '!seen[$1 FS $2]++' "$plan" >"$plan.tmp" && mv "$plan.tmp" "$plan"

echo
echo "plan:"
awk -F'\t' '{ n[$1]++; b[$1] += $4 } END { for (k in n) printf "  %-9s %6d items %14d bytes\n", k, n[k], b[k] }' "$plan" | sort
echo "  (full list: $plan)"
if ! $apply; then
	echo
	echo "dry run; add --apply to delete"
	exit 0
fi
if [ ! -s "$plan" ]; then
	echo "nothing to delete"
	exit 0
fi
# A classic token lists its scopes in a header; a fine-grained token has none,
# and a failed probe leaves the check to the first DELETE instead.
scopes="$(gh api -i user 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1) == "x-oauth-scopes" { print $2 }' || true)"
if [ -n "$scopes" ] && $scope_packages && ! grep -q -F -- 'delete:packages' <<<"$scopes"; then
	echo "the token lacks delete:packages (has: $scopes); run: gh auth refresh -s delete:packages" >&2
	exit 1
fi
if ! $yes; then
	echo
	read -r -p "type the repository name ($repo) to delete everything in the plan: " answer </dev/tty
	[ "$answer" = "$repo" ] || { echo "aborted"; exit 1; }
fi

: >"$out/deleted.tsv"
: >"$out/failed.tsv"
done_count=0
# Assets before releases, versions before packages, so nothing is deleted twice.
for kind in asset release version package artifact cache; do
	while IFS=$'\t' read -r k id label bytes path; do
		[ "$k" = "$kind" ] || continue
		if api -X DELETE "$path" >/dev/null; then
			printf '%s\t%s\t%s\t%s\n' "$kind" "$id" "$label" "$bytes" >>"$out/deleted.tsv"
			echo "deleted $kind $label"
		else
			printf '%s\t%s\t%s\t%s\n' "$kind" "$id" "$label" "$bytes" >>"$out/failed.tsv"
			echo "FAILED  $kind $label" >&2
		fi
		done_count=$((done_count + 1))
		[ $((done_count % 100)) -ne 0 ] || throttle
	done <"$plan"
done
echo
echo "deleted $(wc -l <"$out/deleted.tsv" | tr -d ' '), failed $(wc -l <"$out/failed.tsv" | tr -d ' ') (see $out)"
[ ! -s "$out/failed.tsv" ]
