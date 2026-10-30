# AGENTS.md

`rgbctl` is a Go binary (one dependency: BurntSushi/toml). Lights come from
`~/.config/rgbctl/config.toml` (`RGBCTL_CONFIG` overrides it); no device
name or id belongs in the code or the docs, only in `examples/`.

## Layout

- `main.go`: config, state, colours, the OpenRGB apply logic, the CLI.
- `openrgb.go`: a minimal OpenRGB SDK client (protocol 4): device list, mode,
  zone and device colours. Field layout follows the SDK's
  `RGBController::GetDeviceDescription`.
- `via.go`: the VIA backend over raw hidraw.
- `openrgb-server.service`: headless OpenRGB SDK server (user unit).
- `examples/`: complete configs and a udev rule.

## Rules

- `state.json` is written in place, not renamed: file watchers stop after a
  rename.
- `state.json` and the `link`/`sync` commands are read by bar widgets; keep
  their shape or say so in the release notes.
- Some VIA firmwares only persist a save when exactly one value changed, so
  the VIA backend saves after every value. Keep that.
- One backend failing (OpenRGB server down, keyboard away) must not stop the
  others.

## Testing

`go vet ./... && go build`. With OpenRGB running,
`RGBCTL_CONFIG=examples/<file>.toml ./rgbctl state` checks the config;
`apply`, `set`, `link` and `sync` touch the real lights. To confirm what the
server holds, run `openrgb --client --list-devices` or read the active mode
and zone colours through the SDK.

## Commits

`type(scope): description` in English; body with why, then lowercase
bullets of what changed, then a `Validation:` line with what was actually run.
