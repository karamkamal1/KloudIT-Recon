#!/usr/bin/env bash
# Vendors github.com/quic-go/quic-go into third_party/quic-go with
# third_party/quic-go.patch applied (see third_party/README.md).
#
#   third_party/update-quic-go.sh <version>|latest
#       Rebase the patch onto that upstream release (a 3-way merge, like
#       git rebase), re-vendor it, and refresh quic-go.patch, NOTICE and the
#       quic-go version in go.mod. Fails, listing the conflicts, when the
#       patch no longer applies.
#   third_party/update-quic-go.sh --check
#       Verify that third_party/quic-go is the NOTICE version + quic-go.patch.
#   third_party/update-quic-go.sh --diff
#       Regenerate quic-go.patch from edits made in third_party/quic-go.
set -euo pipefail

mod=github.com/quic-go/quic-go
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(dirname "$here")
dst=$here/quic-go
patch=$here/quic-go.patch

die() {
	echo "update-quic-go: $*" >&2
	exit 1
}

[ $# -eq 1 ] || die "usage: $0 <version>|latest|--check|--diff"
mode=$1
command -v git >/dev/null || die "git is required"
command -v go >/dev/null || die "go is required"
[ -f "$dst/NOTICE" ] || die "$dst/NOTICE is missing"
cur=$(sed -n 's/^Version: //p' "$dst/NOTICE")
[ -n "$cur" ] || die "no Version line in $dst/NOTICE"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
repo=$work/repo
export GOFLAGS= GOWORK=off

g() {
	git -C "$repo" -c user.name=update-quic-go -c user.email=update-quic-go@localhost \
		-c commit.gpgsign=false -c core.autocrlf=false "$@"
}

# download <version>: sets version, hash (upstream commit) and dir (module source).
download() {
	local info
	info=$(cd "$work" && go mod download -json "$mod@$1") || die "cannot download $mod@$1"
	version=$(printf '%s\n' "$info" | grep -o '"Version": "[^"]*"' | head -n1 | cut -d'"' -f4)
	hash=$(printf '%s\n' "$info" | grep -o '"Hash": "[^"]*"' | head -n1 | cut -d'"' -f4 || true)
	dir=$(printf '%s\n' "$info" | grep -o '"Dir": "[^"]*"' | head -n1 | cut -d'"' -f4)
	[ -n "$dir" ] && [ -d "$dir" ] || die "no source directory for $mod@$1"
}

# copytree <from> <to>: copies a directory's contents (without .git), writable.
copytree() {
	(cd "$1" && tar -cf - --exclude=./.git .) | (cd "$2" && tar -xf -)
	chmod -R u+w "$2"
}

# load <dir>: makes <dir> the repository's tree and commits it as <message>.
load() {
	g rm -rqf --ignore-unmatch .
	copytree "$1" "$repo"
	g add -Af .
	g commit -q --allow-empty -m "$2"
}

diffpatch() {
	g diff --no-color --no-ext-diff --no-renames --src-prefix=a/ --dst-prefix=b/ "$1" "$2"
}

writenotice() { # <dir> <version> <commit>
	cat >"$1/NOTICE" <<EOF
This directory is a modified copy of github.com/quic-go/quic-go (MIT License,
see LICENSE), used through a replace directive in the repository's go.mod.

Upstream: https://github.com/quic-go/quic-go
Version: $2
Commit: ${3:-unknown}

Modifications: ../quic-go.patch (a pluggable congestion controller:
quic.Config.Congestion, (*quic.Conn).CongestionControl and the public package
github.com/quic-go/quic-go/congestion; SendStream.SetReliableBoundary is a
no-op once the stream was reset). Edit the files here, then run
../update-quic-go.sh --diff to refresh the patch; ../update-quic-go.sh <version>
moves to another upstream release. See ../README.md.
EOF
}

mkdir -p "$repo"
git init -q "$repo"
download "$cur"
cur_hash=$hash
load "$dir" "upstream $cur"
base=$(g rev-parse HEAD)

if [ "$mode" = --diff ]; then
	load "$dst" "edited"
	g rm -qf NOTICE
	g commit -q --allow-empty -m "without NOTICE"
	diffpatch "$base" HEAD >"$patch"
	echo "wrote $patch ($(grep -c '^diff --git' "$patch") files)"
	exit 0
fi

g apply --whitespace=nowarn "$patch" || die "quic-go.patch does not apply to $mod $cur"
g add -Af .
g commit -q -m "kloudit-recon patch"
patched=$(g rev-parse HEAD)

if [ "$mode" = --check ]; then
	writenotice "$repo" "$cur" "$cur_hash"
	if ! diff -r -q -x .git "$repo" "$dst"; then
		die "third_party/quic-go is not $mod $cur + quic-go.patch (after editing the fork, run $0 --diff)"
	fi
	req=$(cd "$root" && go list -m -f '{{.Version}}' "$mod")
	[ "$req" = "$cur" ] || die "go.mod requires $mod $req, but $cur is vendored"
	echo "third_party/quic-go is $mod $cur + quic-go.patch"
	exit 0
fi
case "$mode" in -*) die "unknown option $mode" ;; esac

download "$mode"
new=$version
newbase=$base
if [ "$new" != "$cur" ]; then
	g checkout -q -b upstream "$base"
	load "$dir" "upstream $new"
	newbase=$(g rev-parse HEAD)
	if ! g cherry-pick "$patched" >"$work/pick.log" 2>&1; then
		cat "$work/pick.log" >&2
		echo >&2
		echo "Conflicting files:" >&2
		g diff --name-only --diff-filter=U >&2
		g diff >&2 || true
		die "quic-go.patch no longer applies to $mod $new: port it by hand (see third_party/README.md)"
	fi
fi

diffpatch "$newbase" HEAD >"$patch"
rm -rf "$dst"
mkdir -p "$dst"
copytree "$repo" "$dst"
writenotice "$dst" "$new" "$hash"
(cd "$root" && go mod edit -require="$mod@$new" && go mod tidy)
echo "vendored $mod $new (was $cur) with quic-go.patch; run the tests listed in third_party/README.md"
