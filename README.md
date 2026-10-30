# rgbctl

One RGB state for every light in the machine: devices behind a headless
[OpenRGB](https://openrgb.org) SDK server (motherboard, RAM, fan
controllers, strips) and keyboards that speak VIA (QMK). A single Go binary
with its own SDK client; each change takes milliseconds, because the server
keeps OpenRGB's slow detection out of the way.

```sh
rgbctl set on true brightness 80 effect static color theme
rgbctl set color '#ff2020' group.ram false
rgbctl toggle
rgbctl sync                # push the state to the VIA keyboards
rgbctl link <group>        # wired | wireless | absent
rgbctl identify <device>   # light each zone of a device alone, to map strips
rgbctl modes               # each OpenRGB device's hardware modes
```

- **Effects**: `static` (direct mode, per zone), and hardware effects that
  most controllers share under different names: `breathing`, `flashing`,
  `spectrum` (the whole device cycles colours), `rainbow` (a moving wave),
  `chase`, `twinkle` and, on keyboards, `reactive`. Each maps to a list of
  OpenRGB mode names (`[openrgb.modes]` overrides them; `rgbctl modes` lists
  what each device has) and to VIA effect numbers per keyboard. A light
  without the effect shows static.
  Most controllers have no hardware brightness, so brightness scales the
  colour (not the rainbow).
- **`color theme`** reads `accent` from a theme colours file (TOML), with
  saturation pushed up because pastel accents wash out on LEDs. Run
  `rgbctl apply` from your desktop's theme-change hook to follow it.
- **Groups** are named sets of lights: an OpenRGB device, or one zone of it,
  or a VIA keyboard. Each can be switched off on its own.
- **VIA keyboards** get brightness, effect, speed and colour on the RGB
  matrix channel, written (and saved to EEPROM) only when they change.

## Configure

`~/.config/rgbctl/config.toml` (`RGBCTL_CONFIG` overrides it):

```toml
theme_colors = "~/.config/my-theme/colors.toml"   # any TOML with `accent = "#rrggbb"`

[[group]]
name = "case"
label = "Case strip"
backend = "openrgb"
device = "Smart Device"   # substring of the OpenRGB device name
zone = 0                  # leave out for the whole device

[[group]]
name = "keyboard"
backend = "via"
hid_ids = ["0003:0000VVVV:0000PPPP"]   # from /sys/class/hidraw/*/device/uevent; first = wired
channel = 3                            # qmk_rgb_matrix
effects = { static = 1, breathing = 2, rainbow = 5 }   # the board's VIA effect numbers; missing ones show static
```

A VIA keyboard needs read/write access to its hidraw node, through a udev
rule like [`examples/70-via-keyboard.rules`](examples/70-via-keyboard.rules) with
your keyboard's vendor id.

State lives in `~/.local/state/rgbctl/state.json` (with the group labels),
which bar widgets can watch.

## Install

```sh
sh install.sh      # builds with Go, enables the OpenRGB server user service
```

## Examples

- [`aura-nzxt-keychron.toml`](examples/aura-nzxt-keychron.toml): a
  motherboard, RAM, a fan controller with two strips, and a VIA keyboard by
  cable or through its 2.4G dongle.

## License

MIT
