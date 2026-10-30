#!/bin/sh
# Build and install rgbctl for your user, with the headless OpenRGB server as
# a user service. Needs Go to build, OpenRGB installed, and i2c-dev loaded for
# motherboard and RAM lights.
set -e
cd "$(dirname "$0")"
go build -trimpath -o rgbctl .
install -Dm755 rgbctl "$HOME/.local/bin/rgbctl"
install -Dm644 openrgb-server.service "$HOME/.config/systemd/user/openrgb-server.service"
systemctl --user daemon-reload
systemctl --user enable --now openrgb-server.service
if [ ! -f "$HOME/.config/rgbctl/config.toml" ]; then
  echo "Write ~/.config/rgbctl/config.toml (start from examples/), then: rgbctl apply"
else
  echo "rgbctl installed. Try: rgbctl state"
fi
