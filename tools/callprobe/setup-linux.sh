#!/usr/bin/env bash
# setup-linux.sh [up|down]: the measuring laptop's devices for a test call,
# made with pactl on PulseAudio or PipeWire (pipewire-pulse). callprobe setup
# runs this script.
#
#   callprobe-mic       a null sink that callprobe play feeds. Meet's
#                       microphone is its monitor, through the source
#                       callprobe-mic-in: Chrome lists no monitor source as a
#                       microphone.
#   callprobe-loopback  a null sink that is Meet's speaker; callprobe record
#                       reads its monitor.
#
# up (the default) makes each device that is missing and changes nothing
# that is there. down unloads the modules that made them, the source that
# reads callprobe-mic's monitor first, and no other.
#
# The script uses bash's own commands and pactl alone.
set -euo pipefail

# listed KIND NAME: whether pactl lists a KIND (sinks or sources) named NAME.
listed() {
	local listing index name rest
	listing=$(pactl list short "$1")
	while IFS=$'\t' read -r index name rest; do
		[[ $name == "$2" ]] && return 0
	done <<<"$listing"
	return 1
}

# ensure KIND NAME MODULE ARGUMENTS...: load MODULE unless KIND NAME is listed.
ensure() {
	local kind=$1 name=$2
	shift 2
	if listed "$kind" "$name"; then
		echo "kept $name"
		return
	fi
	pactl load-module "$@" >/dev/null
	listed "$kind" "$name" || { echo "setup-linux.sh: pactl loaded $1 but lists no $name" >&2; exit 1; }
	echo "made $name"
}

up() {
	ensure sinks callprobe-mic module-null-sink sink_name=callprobe-mic rate=48000 channels=1 \
		sink_properties=device.description=callprobe-mic
	ensure sources callprobe-mic-in module-remap-source master=callprobe-mic.monitor source_name=callprobe-mic-in \
		source_properties=device.description=callprobe-mic-in
	ensure sinks callprobe-loopback module-null-sink sink_name=callprobe-loopback rate=48000 channels=1 \
		sink_properties=device.description=callprobe-loopback
	echo "In Meet's settings: microphone callprobe-mic-in, speaker callprobe-loopback."
}

# ours MODULE ARGUMENTS: whether the module is one that up loads.
ours() {
	local words word
	read -ra words <<<"$2"
	for word in "${words[@]}"; do
		case "$1 $word" in
		"module-remap-source source_name=callprobe-mic-in" | \
			"module-null-sink sink_name=callprobe-mic" | \
			"module-null-sink sink_name=callprobe-loopback") return 0 ;;
		esac
	done
	return 1
}

down() {
	local listing index module args pass removed=0
	listing=$(pactl list short modules)
	for pass in module-remap-source module-null-sink; do
		while IFS=$'\t' read -r index module args; do
			if [[ $module != "$pass" ]] || ! ours "$module" "$args"; then
				continue
			fi
			pactl unload-module "$index"
			echo "removed module $index ($module $args)"
			removed=$((removed + 1))
		done <<<"$listing"
	done
	[[ $removed -gt 0 ]] || echo "no callprobe device to remove"
}

case "${1:-up}" in
up) up ;;
down) down ;;
*)
	echo "usage: setup-linux.sh [up|down]" >&2
	exit 2
	;;
esac
