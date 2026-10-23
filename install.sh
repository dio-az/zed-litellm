#!/bin/sh
# Installs zed-litellm as a macOS launchd agent, or removes it.
#
#   sh install.sh              Download the latest release, ask for settings, and start the agent.
#   sh install.sh <binary>     Install a local build instead. `just install` does this.
#   sh install.sh --uninstall  Stop the agent, and remove the binary and the agent.
set -eu

repo=dio-az/zed-litellm
name=zed-litellm
label=dev.dio-az.$name
bin_dir=$HOME/.local/bin
bin=$bin_dir/$name
config=$HOME/.config/$name/config.json
state=$HOME/.local/state/$name
error_log=$state/error.log
plist=$HOME/Library/LaunchAgents/$label.plist
domain=gui/$(id -u)
default_listen=127.0.0.1:8787

say() { printf '%s\n' "$*"; }
fail() {
	say "error: $*" >&2
	exit 1
}

# Paths print with ~, the way a user types them.
short() {
	# shellcheck disable=SC2088 # The ~ is for display.
	case $1 in
	"$HOME"/*) printf '~/%s' "${1#"$HOME"/}" ;;
	*) printf '%s' "$1" ;;
	esac
}

# ask PROMPT DEFAULT reads the answer from the terminal.
ask() {
	if [ -n "$2" ]; then
		printf '%s [%s]: ' "$1" "$2" >/dev/tty
	else
		printf '%s: ' "$1" >/dev/tty
	fi
	if ! read -r answer </dev/tty; then
		answer=
		printf '\n' >/dev/tty
	fi
	printf '%s' "${answer:-$2}"
}

[ "$(uname -s)" = Darwin ] || fail "macOS only"

if [ "${1:-}" = --uninstall ]; then
	launchctl bootout "$domain/$label" 2>/dev/null || true
	rm -f "$plist" "$bin"
	say "Removed zed-litellm. To also remove its config and logs:"
	say "  rm -rf $(short "$(dirname "$config")") $(short "$state")"
	exit 0
fi

mkdir -p "$bin_dir" "$(dirname "$plist")" "$state"
if [ -n "${1:-}" ]; then
	[ -f "$1" ] || fail "$1 not found"
	install -m 755 "$1" "$bin"
else
	case $(uname -m) in
	arm64) arch=arm64 ;;
	x86_64) arch=amd64 ;;
	*) fail "unsupported CPU: $(uname -m)" ;;
	esac
	asset=${name}_darwin_$arch
	url=https://github.com/$repo/releases/latest/download
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	say "Downloading $asset..."
	curl -fsSL -o "$tmp/$asset" "$url/$asset"
	curl -fsSL -o "$tmp/SHA256SUMS" "$url/SHA256SUMS"
	sum=$(grep " $asset\$" "$tmp/SHA256SUMS") || fail "$asset missing from SHA256SUMS"
	(cd "$tmp" && printf '%s\n' "$sum" | shasum -a 256 -c -s) || fail "checksum mismatch: $asset"
	install -m 755 "$tmp/$asset" "$bin"
fi

(: </dev/tty) 2>/dev/null || fail "run this in a terminal"

write_config=yes
if [ -f "$config" ]; then
	case $(ask "Keep $(short "$config")?" Y/n) in
	[Nn]*) mv "$config" "$config.bak" && say "Moved the old config to $(short "$config.bak")" ;;
	*) write_config=no ;;
	esac
fi
if [ "$write_config" = yes ]; then
	upstream=
	while [ -z "$upstream" ]; do
		upstream=$(ask "LiteLLM URL (e.g. https://litellm.example.com)" "")
	done
	if ! curl -fsS -m 5 -o /dev/null "${upstream%/}/health/liveliness" 2>/dev/null; then
		say "warning: no answer from ${upstream%/}/health/liveliness"
	fi
	listen=$(ask "Listen address" "$default_listen")
	while :; do
		cache=$(ask "Cache TTL (auto, 1h, bypass)" auto)
		case $cache in auto | 1h | bypass) break ;; esac
	done
	# Answers equal to the defaults stay out of the config, so a new default in a later release
	# applies.
	set -- -upstream "$upstream"
	[ "$listen" = "$default_listen" ] || set -- "$@" -listen "$listen"
	[ "$cache" = auto ] || set -- "$@" -cache "$cache"
	"$bin" "$@" init >/dev/null
fi

cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$label</string>
	<key>ProgramArguments</key>
	<array>
		<string>$bin</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardErrorPath</key>
	<string>$error_log</string>
</dict>
</plist>
EOF

# bootout returns before launchd removes the old agent, and bootstrap fails until then.
launchctl bootout "$domain/$label" 2>/dev/null || true
i=0
while launchctl print "$domain/$label" >/dev/null 2>&1 && [ "$i" -lt 50 ]; do
	sleep 0.1
	i=$((i + 1))
done
launchctl bootstrap "$domain" "$plist"

listen=$(sed -n 's/.*"listen": *"\([^"]*\)".*/\1/p' "$config")
say ""
say "zed-litellm is running."
say "  Config:     $(short "$config")"
say "  Error logs: $(short "$error_log")"
say ""
say "Update Zed settings with:"
say "  \"language_models\": { \"anthropic\": { \"api_url\": \"http://${listen:-$default_listen}\" } }"
