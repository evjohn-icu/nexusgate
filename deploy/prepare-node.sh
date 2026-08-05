#!/bin/sh
# Prepare a machine to run a Timingdex Hub or Worker.
#
# The setup script the Hub generates downloads the binary and enrols it; this
# one covers what has to be true before that works. It is separate because it
# needs root and the enrolment script does not, and because it is worth running
# on its own to find out what a machine can actually do.
#
# What it does:
#   * reports the addresses a Hub could reach this node on
#   * detects the distribution and the GPU
#   * installs ffmpeg, ffprobe and exiftool, plus the video driver for that GPU
#   * adds the invoking user to the groups that own /dev/dri
#   * verifies by encoding one frame with each accelerator, which is the only
#     check that distinguishes "installed" from "works"
#
# Nothing is installed before the plan is printed. Re-running is safe.
#
# Usage:
#   ./prepare-node.sh              print the plan, ask before installing
#   ./prepare-node.sh --yes        install without asking
#   ./prepare-node.sh --check      report only, change nothing
set -eu

ASSUME_YES=0
CHECK_ONLY=0
for arg in "$@"; do
	case "$arg" in
	--yes | -y) ASSUME_YES=1 ;;
	--check | -n | --dry-run) CHECK_ONLY=1 ;;
	--help | -h)
		sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown option: $arg (try --help)" >&2
		exit 2
		;;
	esac
done

say() { printf '%s\n' "$*"; }
head2() { printf '\n=== %s ===\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

SUDO=""
SUDO_NEEDS_PASSWORD=0
if [ "$(id -u)" -ne 0 ]; then
	if have sudo; then
		SUDO="sudo"
		# Worth knowing before the plan is printed rather than half way through
		# an install: over a non-interactive connection sudo cannot prompt, and
		# its own failure is easy to mistake for a broken script.
		sudo -n true >/dev/null 2>&1 || SUDO_NEEDS_PASSWORD=1
	else
		say "This needs root and sudo is not installed; re-run as root." >&2
		exit 1
	fi
fi

# ---------------------------------------------------------------- addresses --
# A Worker dials the Hub, so a Worker's own address does not need to be
# reachable. It is still worth printing: on a NAS or a box with several
# interfaces, knowing which address is which is what makes the Hub URL
# obvious, and it identifies the node in the Hub's worker list.
head2 "addresses"
if have ip; then
	ip -4 -o addr show scope global 2>/dev/null |
		awk '{split($4,a,"/"); printf "  %-10s %s\n", $2, a[1]}' |
		grep -v -E '  (docker|br-|veth|virbr)' || say "  (none found)"
elif have ifconfig; then
	ifconfig 2>/dev/null | awk '/inet /{print "  " $2}' | grep -v 127.0.0.1 || say "  (none found)"
else
	say "  (no ip/ifconfig available)"
fi
say "  hostname: $(hostname 2>/dev/null || echo unknown)"

# -------------------------------------------------------------- environment --
head2 "environment"
OS="$(uname -s)"
DISTRO="unknown"
DISTRO_LIKE=""
if [ -r /etc/os-release ]; then
	# shellcheck disable=SC1091
	. /etc/os-release
	DISTRO="${ID:-unknown}"
	DISTRO_LIKE="${ID_LIKE:-}"
	say "  system:   ${PRETTY_NAME:-$DISTRO}"
else
	say "  system:   $OS"
fi
say "  kernel:   $(uname -r)"
say "  cpu:      $(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo '?') cores"

PKG=""
case "$DISTRO $DISTRO_LIKE" in
*debian* | *ubuntu*) PKG="apt" ;;
*fedora* | *rhel* | *centos*) PKG="dnf" ;;
*arch*) PKG="pacman" ;;
*alpine*) PKG="apk" ;;
esac
if [ "$OS" = "Darwin" ]; then PKG="brew"; fi
if [ -z "$PKG" ]; then
	say "  packages: unrecognised distribution — install the tools below by hand"
else
	say "  packages: $PKG"
fi

# ---------------------------------------------------------------------- gpu --
head2 "gpu"
GPU="none"
GPU_DETAIL=""
if [ -e /dev/nvidia0 ] || have nvidia-smi || [ -e /usr/lib/wsl/lib/libnvidia-encode.so.1 ]; then
	GPU="nvidia"
	if have nvidia-smi; then
		GPU_DETAIL="$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)"
	elif [ -x /usr/lib/wsl/lib/nvidia-smi ]; then
		GPU_DETAIL="$(/usr/lib/wsl/lib/nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)"
	fi
elif [ -d /dev/dri ] && ls /dev/dri/renderD* >/dev/null 2>&1; then
	VENDOR=""
	if have lspci; then
		VENDOR="$(lspci -nn 2>/dev/null | grep -Ei 'vga|display|3d' | head -1)"
	fi
	case "$VENDOR" in
	*Intel* | *intel*) GPU="intel" ;;
	*AMD* | *ATI* | *Advanced\ Micro*) GPU="amd" ;;
	*) GPU="dri" ;;
	esac
	GPU_DETAIL="$VENDOR"
elif [ "$OS" = "Darwin" ]; then
	GPU="videotoolbox"
fi
say "  detected: $GPU"
[ -n "$GPU_DETAIL" ] && say "  device:   $GPU_DETAIL"
if [ -d /dev/dri ]; then
	ls -l /dev/dri/ 2>/dev/null | awk 'NR>1{printf "  %s %s %s\n", $1, $4, $NF}'
fi

# ------------------------------------------------------------------ missing --
head2 "what is missing"
NEED_TOOLS=""
for tool in ffmpeg ffprobe exiftool; do
	if have "$tool"; then
		say "  ok       $tool"
	else
		say "  MISSING  $tool"
		NEED_TOOLS="$NEED_TOOLS $tool"
	fi
done

# A device node that exists but cannot be opened is the failure that looks like
# working hardware: every static check passes and every encode fails.
NEED_GROUPS=""
if [ "$GPU" = "intel" ] || [ "$GPU" = "amd" ] || [ "$GPU" = "dri" ]; then
	for node in /dev/dri/renderD*; do
		[ -e "$node" ] || continue
		if [ -r "$node" ] && [ -w "$node" ]; then
			say "  ok       $node is readable and writable"
		else
			owner_group="$(stat -c '%G' "$node" 2>/dev/null || echo render)"
			say "  MISSING  $node needs group '$owner_group' — $(id -un) is not in it"
			case " $NEED_GROUPS " in
			*" $owner_group "*) ;;
			*) NEED_GROUPS="$NEED_GROUPS $owner_group" ;;
			esac
		fi
	done
fi

# ---------------------------------------------------------------- packages ---
# Whether the distribution can actually supply a package. A name that is not
# carried here should not appear in the plan at all: proposing an install that
# can never succeed makes the plan untrustworthy, and these driver names differ
# between distributions and releases often enough for that to be common.
pkg_available() {
	case "$PKG" in
	apt) apt-get install -s -y "$1" >/dev/null 2>&1 ;;
	dnf) dnf --quiet info "$1" >/dev/null 2>&1 ;;
	pacman) pacman -Si "$1" >/dev/null 2>&1 ;;
	apk) apk info --description "$1" >/dev/null 2>&1 ;;
	brew) brew info --formula "$1" >/dev/null 2>&1 ;;
	*) return 1 ;;
	esac
}

pkg_installed() {
	case "$PKG" in
	apt) dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "ok installed" ;;
	dnf) rpm -q "$1" >/dev/null 2>&1 ;;
	pacman) pacman -Q "$1" >/dev/null 2>&1 ;;
	apk) apk info -e "$1" >/dev/null 2>&1 ;;
	brew) brew list --versions "$1" >/dev/null 2>&1 ;;
	*) return 1 ;;
	esac
}

DRIVER_PKGS=""
TOOL_PKGS=""
NONFREE_NOTE=""
case "$PKG" in
apt)
	case "$NEED_TOOLS" in *ffmpeg* | *ffprobe*) TOOL_PKGS="$TOOL_PKGS ffmpeg" ;; esac
	case "$NEED_TOOLS" in *exiftool*) TOOL_PKGS="$TOOL_PKGS libimage-exiftool-perl" ;; esac
	case "$GPU" in
	intel)
		# Both driver names are offered: the non-free build needs a component
		# many installs do not enable, and the free one is usually present and
		# enough. libvpl2 is the oneVPL runtime h264_qsv needs — without it
		# FFmpeg still lists the encoder and every encode fails.
		DRIVER_PKGS="intel-media-va-driver-non-free intel-media-va-driver libvpl2 vainfo"
		NONFREE_NOTE="the non-free Intel driver needs Debian's non-free component; if it is not enabled the free intel-media-va-driver is installed instead, which is usually sufficient."
		;;
	amd) DRIVER_PKGS="mesa-va-drivers vainfo" ;;
	esac
	;;
dnf)
	case "$NEED_TOOLS" in *ffmpeg* | *ffprobe*) TOOL_PKGS="$TOOL_PKGS ffmpeg" ;; esac
	case "$NEED_TOOLS" in *exiftool*) TOOL_PKGS="$TOOL_PKGS perl-Image-ExifTool" ;; esac
	case "$GPU" in
	intel) DRIVER_PKGS="intel-media-driver libva-utils" ;;
	amd) DRIVER_PKGS="mesa-va-drivers libva-utils" ;;
	esac
	;;
pacman)
	case "$NEED_TOOLS" in *ffmpeg* | *ffprobe*) TOOL_PKGS="$TOOL_PKGS ffmpeg" ;; esac
	case "$NEED_TOOLS" in *exiftool*) TOOL_PKGS="$TOOL_PKGS perl-image-exiftool" ;; esac
	case "$GPU" in
	intel) DRIVER_PKGS="intel-media-driver libva-utils" ;;
	amd) DRIVER_PKGS="libva-mesa-driver libva-utils" ;;
	esac
	;;
apk)
	case "$NEED_TOOLS" in *ffmpeg* | *ffprobe*) TOOL_PKGS="$TOOL_PKGS ffmpeg" ;; esac
	case "$NEED_TOOLS" in *exiftool*) TOOL_PKGS="$TOOL_PKGS exiftool" ;; esac
	case "$GPU" in
	intel) DRIVER_PKGS="intel-media-driver libva-utils" ;;
	amd) DRIVER_PKGS="mesa-va-gallium libva-utils" ;;
	esac
	;;
brew)
	case "$NEED_TOOLS" in *ffmpeg* | *ffprobe*) TOOL_PKGS="$TOOL_PKGS ffmpeg" ;; esac
	case "$NEED_TOOLS" in *exiftool*) TOOL_PKGS="$TOOL_PKGS exiftool" ;; esac
	;;
esac

# A driver already present is not part of the plan. Without this a prepared
# node keeps reporting work to do, which makes --check useless as a way to ask
# whether a machine is ready.
PENDING_DRIVERS=""
for pkg in $DRIVER_PKGS; do
	pkg_installed "$pkg" && continue
	pkg_available "$pkg" || continue
	PENDING_DRIVERS="$PENDING_DRIVERS $pkg"
done
DRIVER_PKGS="$PENDING_DRIVERS"

PLAN=""
[ -n "$TOOL_PKGS" ] && PLAN="$TOOL_PKGS"
[ -n "$DRIVER_PKGS" ] && PLAN="$PLAN $DRIVER_PKGS"

head2 "plan"
if [ -z "$PLAN" ] && [ -z "$NEED_GROUPS" ]; then
	say "  nothing to do — this node already has what it needs"
else
	[ -n "$PLAN" ] && say "  install: $PLAN"
	[ -n "$NEED_GROUPS" ] && say "  add $(id -un) to group(s):$NEED_GROUPS"
	case "$DRIVER_PKGS" in *non-free*) [ -n "$NONFREE_NOTE" ] && say "  note: $NONFREE_NOTE" ;; esac
	if [ "$SUDO_NEEDS_PASSWORD" -eq 1 ]; then
		say ""
		say "  sudo will ask for a password, and cannot do so without a terminal."
		say "  Run this from a login shell on the machine itself, not over a"
		say "  non-interactive connection."
	fi
fi

# ------------------------------------------------------------------ verify ---
# Listing an encoder is not the same as being able to run it: a driver older
# than the build's API, a device without permission, and a missing firmware
# blob all list fine and fail on the first frame. Encoding one frame is what
# separates them, and it is the same check the Hub makes when it picks a plan.
#
# LIBVA_DRIVER_NAME is part of the probe rather than advice printed afterwards.
# libva maps the kernel driver to a userspace one, and on Debian- and
# Ubuntu-derived releases i915 still resolves to i965, which supports nothing
# from Gen12 onward. A current Intel iGPU — what a NAS has — therefore fails
# every encode until the driver is named, with no error that says so. The Hub
# probes the same list in the same order, so this agrees with what it selects.
#
# The override runs in a subshell so that an empty driver means "inherit",
# not "set LIBVA_DRIVER_NAME to nothing" — libva reads the variable's presence,
# so an empty value is a request for a driver named "" and fails.
probe_once() {
	backend="$1"
	encoder="$2"
	driver="$3"
	(
		if [ -n "$driver" ]; then
			LIBVA_DRIVER_NAME="$driver"
			export LIBVA_DRIVER_NAME
		fi
		case "$backend" in
		vaapi)
			ffmpeg -hide_banner -loglevel error -y \
				-vaapi_device /dev/dri/renderD128 \
				-f lavfi -i "color=c=black:s=256x144:r=25:d=0.2" -frames:v 1 \
				-vf format=nv12,hwupload -c:v "$encoder" -f null -
			;;
		*)
			ffmpeg -hide_banner -loglevel error -y \
				-f lavfi -i "color=c=black:s=256x144:r=25:d=0.2" -frames:v 1 \
				-c:v "$encoder" -f null -
			;;
		esac
	) >/dev/null 2>&1
}

# Sets PROBE_DRIVER to the driver that worked, empty when libva's own choice
# was already right.
probe() {
	backend="$1"
	encoder="$2"
	PROBE_DRIVER=""
	have ffmpeg || return 1
	# "default" stands for libva's own choice; an empty word cannot survive the
	# unquoted expansion this loop relies on.
	case "$backend" in
	vaapi | qsv) drivers="default iHD i965" ;;
	*) drivers="default" ;;
	esac
	for driver in $drivers; do
		if [ "$driver" = "default" ]; then
			driver=""
		fi
		if probe_once "$backend" "$encoder" "$driver"; then
			PROBE_DRIVER="$driver"
			return 0
		fi
	done
	return 1
}

verify() {
	head2 "verification"
	for tool in ffmpeg ffprobe exiftool; do
		if have "$tool"; then
			say "  ok       $tool"
		else
			say "  MISSING  $tool — the pipeline cannot run without it"
		fi
	done

	if have ffmpeg; then
		for pair in "cuda h264_nvenc" "qsv h264_qsv" "vaapi h264_vaapi" "videotoolbox h264_videotoolbox" "software libx264"; do
			backend="${pair%% *}"
			encoder="${pair##* }"
			if ! ffmpeg -hide_banner -encoders 2>/dev/null | grep -q "$encoder"; then
				continue
			fi
			if probe "$backend" "$encoder"; then
				if [ -n "$PROBE_DRIVER" ]; then
					say "  ok       $encoder encodes here (needs LIBVA_DRIVER_NAME=$PROBE_DRIVER)"
				else
					say "  ok       $encoder encodes here"
				fi
			else
				say "  no       $encoder is built in but does not run here"
			fi
		done
	fi
}

if [ "$CHECK_ONLY" -eq 1 ]; then
	verify
	say ""
	say "--check given; nothing was changed."
	exit 0
fi

if [ -n "$PLAN" ] || [ -n "$NEED_GROUPS" ]; then
	if [ "$ASSUME_YES" -ne 1 ]; then
		printf '\nProceed? [y/N]: '
		read -r reply
		case "$reply" in
		y | Y | yes | YES) ;;
		*)
			say "Aborted."
			exit 0
			;;
		esac
	fi

	head2 "installing"
	# shellcheck disable=SC2086
	install_pkgs() {
		case "$PKG" in
		apt) DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -y "$@" ;;
		dnf) $SUDO dnf install -y "$@" ;;
		pacman) $SUDO pacman -S --noconfirm --needed "$@" ;;
		apk) $SUDO apk add --no-cache "$@" ;;
		brew) brew install "$@" ;;
		*) return 1 ;;
		esac
	}

	if [ "$PKG" = "apt" ]; then
		$SUDO apt-get update -qq || say "  warning: package lists could not be refreshed"
	fi

	# The tools are required; failing to install them is a real failure.
	if [ -n "$TOOL_PKGS" ]; then
		# shellcheck disable=SC2086
		if ! install_pkgs $TOOL_PKGS; then
			say "  ERROR: could not install $TOOL_PKGS — the pipeline needs these" >&2
			exit 1
		fi
	fi

	# Drivers are best-effort and installed one at a time. Distributions rename
	# and re-license these packages constantly, and a name that does not exist
	# here must not stop the rest — including, before this was split, the tools
	# above, which shared the transaction and were lost with it.
	for pkg in $DRIVER_PKGS; do
		if install_pkgs "$pkg" >/dev/null 2>&1; then
			say "  installed $pkg"
		else
			say "  skipped   $pkg (not available on this system)"
		fi
	done
	for group in $NEED_GROUPS; do
		$SUDO usermod -aG "$group" "$(id -un)" && say "  added $(id -un) to $group"
	done
fi

verify

if [ -n "$NEED_GROUPS" ]; then
	say ""
	say "Group membership was changed. It does not apply to this shell — log out"
	say "and back in (or reconnect over SSH), then re-run with --check to confirm."
fi

head2 "next"
say "  This node is prepared. Enrol it from the Hub's /workers page, which"
say "  issues a one-time pairing token and generates the enrolment command."
