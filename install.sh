#!/bin/sh
# Installs Artus on macOS or Linux:
#
#   curl -fsSL https://raw.githubusercontent.com/pschachhuber/artus/main/install.sh | sh
#
# Downloads the program for this system from the latest GitHub Release, checks
# it against the release's checksums.txt and puts it in ~/.local/bin, a folder
# of your own, so that Artus can update itself there. Run it again to reinstall.
#
#   ARTUS_INSTALL_DIR  install somewhere else
#   ARTUS_VERSION      install a given release, such as v0.1.0
#
# A file fetched with curl is not marked as downloaded from the internet, so
# macOS opens it without the "Apple could not verify …" prompt it shows for an
# unsigned file saved by a browser.
set -eu

fail() {
	echo "artus: $*" >&2
	exit 1
}

# Everything is in a function, so that a download cut off half way runs nothing.
main() {
	repo=pschachhuber/artus
	dir=${ARTUS_INSTALL_DIR:-$HOME/.local/bin}
	version=${ARTUS_VERSION:-latest}

	case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "no build for $(uname -s); on Windows run install.ps1 in PowerShell" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "no build for $(uname -m)" ;;
	esac
	# A shell running under Rosetta reports x86_64 on Apple silicon.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
		arch=arm64
	fi
	asset=artus_${os}_${arch}

	if [ "$version" = latest ]; then
		base=https://github.com/$repo/releases/latest/download
	else
		base=https://github.com/$repo/releases/download/$version
	fi

	if command -v curl >/dev/null 2>&1; then
		fetch() { curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"; }
	elif command -v wget >/dev/null 2>&1; then
		fetch() { wget -q -O "$2" "$1"; }
	else
		fail "curl or wget is needed"
	fi
	if command -v sha256sum >/dev/null 2>&1; then
		sha256() { sha256sum "$1" | cut -d' ' -f1; }
	elif command -v shasum >/dev/null 2>&1; then
		sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
	else
		fail "sha256sum or shasum is needed"
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	echo "Downloading $asset ($version)"
	fetch "$base/$asset" "$tmp/$asset" || fail "cannot download $base/$asset"
	fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "cannot download $base/checksums.txt"
	want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "checksums.txt lists no $asset"
	got=$(sha256 "$tmp/$asset")
	[ "$got" = "$want" ] || fail "$asset does not match its checksum (got $got, want $want); nothing was installed"

	# Copy next to the target and rename, which also replaces a running copy.
	mkdir -p "$dir"
	cp "$tmp/$asset" "$dir/.artus-install"
	chmod 755 "$dir/.artus-install"
	mv -f "$dir/.artus-install" "$dir/artus"

	installed=$("$dir/artus" version) || fail "$dir/artus was installed but does not run here"
	echo "Installed $installed to $dir/artus"
	case ":$PATH:" in
	*":$dir:"*)
		echo "On a server, to use it from your phone and other devices: artus setup"
		echo "On this computer only: artus"
		;;
	*)
		echo "On a server, to use it from your phone and other devices: $dir/artus setup"
		echo "On this computer only: $dir/artus"
		case ${SHELL:-} in
		*/zsh) profile=~/.zshrc ;;
		*/bash) if [ "$os" = darwin ]; then profile=~/.bash_profile; else profile=~/.bashrc; fi ;;
		*) profile=~/.profile ;;
		esac
		echo "To start it as just \"artus\", add $dir to your PATH:"
		echo "  echo 'export PATH=\"$dir:\$PATH\"' >> $profile"
		;;
	esac
}

main "$@"
