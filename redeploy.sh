#!/usr/bin/env bash
# Rebuild + redeploy Spekder (local install on this box).
#
# Runs tests, rebuilds both binaries, and restarts the arena service so it loads
# the new build (including any wire-protocol changes). The door is relaunched
# per-caller by the BBS, so rebuilding it on disk is enough -- only the
# long-running arena server needs a restart.
set -euo pipefail
cd "$(dirname "$0")"

echo "==> go test"
go test ./...

echo "==> build door"
go build -buildvcs=false -o spekder .

echo "==> build server"
go build -buildvcs=false -o spekder-server ./cmd/server

echo "==> restart arena service"
restarted=false
if [ -t 0 ]; then
	# Interactive: let sudo prompt for a password if needed.
	sudo systemctl restart spekder-server && restarted=true
elif sudo -n systemctl restart spekder-server 2>/dev/null; then
	# Non-interactive (CI/agent): only works with passwordless sudo.
	restarted=true
fi
if $restarted; then
	systemctl --no-pager --lines=4 status spekder-server || true
else
	echo "   could not restart automatically (needs root). Run:"
	echo "     sudo systemctl restart spekder-server"
fi
echo "==> done"
