#!/usr/bin/env bash
# Decides whether the checked-out commit becomes a release, and as which
# version. Run by .github/workflows/release.yml; writes `tag=<vX.Y.Z>` to
# $GITHUB_OUTPUT, or `tag=` when there is nothing to release.
#
#   BUMP     auto (default), patch, minor or major
#   FROM_CI  true when called after CI on a push to main: a push that changes
#            nothing in the program (README, CI files, tests, install scripts)
#            is not released
#
# auto raises the patch number, or the minor or major number when a commit
# message since the last release contains [minor] or [major]. The first
# release is v0.1.0.
set -euo pipefail

BUMP=${BUMP:-auto}
FROM_CI=${FROM_CI:-false}

release() {
	echo "tag=$1" >>"${GITHUB_OUTPUT:-/dev/stdout}"
	if [[ -n $1 ]]; then echo "Releasing $1"; fi
	exit 0
}

# A pushed version tag is released as it is.
if [[ ${GITHUB_REF:-} == refs/tags/v* ]]; then
	release "${GITHUB_REF#refs/tags/}"
fi

# A commit that already carries a version tag (a re-run after a failed
# publish) is released under that tag, unless its release is complete.
here=$(git tag --points-at HEAD --list 'v[0-9]*' --sort=-v:refname | head -n1)
if [[ -n $here ]]; then
	if gh release view "$here" --json assets --jq '.assets[].name' 2>/dev/null | grep -qx checksums.txt; then
		echo "$here is already released"
		release ""
	fi
	release "$here"
fi

latest=$(git tag --list 'v[0-9]*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n1 || true)
if [[ -z $latest ]]; then
	if [[ $BUMP == major ]]; then release v1.0.0; fi
	release v0.1.0
fi

# CI runs can finish out of order; a commit a newer release contains is done.
if git merge-base --is-ancestor HEAD "$latest"; then
	echo "This commit is already part of $latest"
	release ""
fi

if [[ $FROM_CI == true ]] && git diff --quiet "$latest" HEAD -- . \
	':(exclude)README.md' ':(exclude)LICENSE' ':(exclude).gitignore' \
	':(exclude)install.sh' ':(exclude)install.ps1' \
	':(exclude).github' ':(glob,exclude)**/*_test.go' ':(glob,exclude)**/testdata/**'; then
	echo "Nothing in the program changed since $latest"
	release ""
fi

if [[ $BUMP == auto ]]; then
	BUMP=patch
	messages=$(git log --format=%B "$latest..HEAD")
	if grep -qiF '[major]' <<<"$messages"; then
		BUMP=major
	elif grep -qiF '[minor]' <<<"$messages"; then
		BUMP=minor
	fi
fi

IFS=. read -r major minor patch <<<"${latest#v}"
case $BUMP in
major) release "v$((major + 1)).0.0" ;;
minor) release "v$major.$((minor + 1)).0" ;;
patch) release "v$major.$minor.$((patch + 1))" ;;
*)
	echo "Unknown bump: $BUMP" >&2
	exit 1
	;;
esac
