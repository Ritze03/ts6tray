#!/bin/sh
# install.sh for ritz-get programs that have a built-in installer (NAME install / NAME uninstall).
# Copy to the repo root, edit the section below.
# ritz-get runs it from inside the clone:  ./install.sh --install --yes   (also for every update)
#                                           ./install.sh --remove  --yes
# By hand, without --yes, the program asks its own questions.
set -eu

# ---- per-program section ---------------------------------------------------------------
NAME=ts6tray
TOOLS="go"                      # tools needed to build, checked with command -v (go cargo npm meson ...)

build() {
	# Go: GOTOOLCHAIN=auto because the pinned Go version is newer than most distro Go packages.
	# It needs any Go >= 1.21 installed and downloads the right toolchain itself.
	GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o "$NAME" .
}
# ----------------------------------------------------------------------------------------

cd "$(dirname "$0")"

missing_tool() {
	f=$(. /etc/os-release 2>/dev/null; echo "${ID:-} ${ID_LIKE:-}")
	t=$1
	case " $f " in
	*" arch "*|*" cachyos "*|*" manjaro "*) c="sudo pacman -S"; [ "$t" = cargo ] && t=rust ;;
	*" fedora "*|*" rhel "*) c="sudo dnf install"; [ "$t" = go ] && t=golang ;;
	*" debian "*|*" ubuntu "*) c="sudo apt install"; [ "$t" = go ] && t=golang-go ;;
	*) c="install with your package manager:" ;;
	esac
	echo "$NAME: '$1' is required to build. Try: $c $t" >&2
	exit 1
}

mode= yes=
for a; do
	case $a in
	--install|--remove) mode=$a ;;
	--yes) yes=--yes ;;
	*) echo "usage: $0 --install | --remove [--yes]" >&2; exit 2 ;;
	esac
done

case $mode in
--install)
	for t in $TOOLS; do command -v "$t" >/dev/null 2>&1 || missing_tool "$t"; done
	build
	"./$NAME" install $yes
	;;
--remove)
	bin=$HOME/.local/bin/$NAME
	[ -x "$bin" ] || bin=./$NAME
	[ -x "$bin" ] || bin=$(command -v "$NAME") || { echo "$NAME is not installed"; exit 0; }
	"$bin" uninstall $yes
	;;
*) echo "usage: $0 --install | --remove [--yes]" >&2; exit 2 ;;
esac
