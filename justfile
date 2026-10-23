bin := "zed-litellm"
label := "dev.dio-az." + bin
domain := "gui/" + `id -u`
plist := home_directory() / "Library/LaunchAgents" / label + ".plist"
error_log := home_directory() / ".local/state" / bin / "error.log"

build:
    go build -trimpath -ldflags="-s -w" -o {{ bin }} .

test:
    go vet ./...
    go test ./...

install: build
    sh install.sh ./{{ bin }}

uninstall:
    sh install.sh --uninstall

start:
    launchctl bootstrap {{ domain }} {{ plist }}

stop:
    launchctl bootout {{ domain }}/{{ label }}

restart:
    launchctl kickstart -k {{ domain }}/{{ label }}

status:
    launchctl print {{ domain }}/{{ label }} | grep -E 'state|pid|last exit'

errors:
    tail {{ error_log }}

# Run a local build with -debug and the given flags. The service holds the port, so stop it first.
[positional-arguments]
debug *flags: build
    ./{{ bin }} -debug "$@"
