// rgbctl keeps one RGB state for every light: OpenRGB SDK server devices and
// VIA keyboards.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const usage = `rgbctl: one RGB state for every light, OpenRGB devices and VIA keyboards.

Lights are grouped in ~/.config/rgbctl/config.toml; the state (on, brightness,
effect, colour, which groups are lit) lives in ~/.local/state/rgbctl/state.json
and every change is applied right away.

Usage: rgbctl apply [--wait]           apply the saved state (--wait: retry until the server is up)
       rgbctl set <key> <value> ...    on true|false, brightness 0-100,
                                       effect static|breathing|rainbow,
                                       color theme|#rrggbb, group.<name> true|false
       rgbctl toggle                   flip on/off
       rgbctl sync [group]             push the state to VIA keyboards
       rgbctl link <group>             print "wired", "wireless" or "absent" for a VIA keyboard
       rgbctl state                    print state as JSON
       rgbctl identify <device>        light each zone of an OpenRGB device alone, to map strips`

var effects = []string{"static", "breathing", "rainbow"}

type Group struct {
	Name    string           `toml:"name"`
	Label   string           `toml:"label"`
	Backend string           `toml:"backend"`
	Device  string           `toml:"device"`
	Zone    *int             `toml:"zone"`
	HIDIDs  []string         `toml:"hid_ids"`
	Channel int              `toml:"channel"`
	Effects map[string]uint8 `toml:"effects"`
	Speed   *uint8           `toml:"speed"`
}

type Config struct {
	ThemeColors string `toml:"theme_colors"`
	OpenRGB     struct {
		Host  string              `toml:"host"`
		Port  int                 `toml:"port"`
		Modes map[string][]string `toml:"modes"`
	} `toml:"openrgb"`
	Groups []Group `toml:"group"`
}

type State struct {
	On         bool                `json:"on"`
	Brightness int                 `json:"brightness"`
	Effect     string              `json:"effect"`
	Color      string              `json:"color"`
	Groups     map[string]bool     `json:"groups"`
	Resolved   string              `json:"resolved"`
	GroupInfo  []map[string]string `json:"group_info"`
}

var (
	home       = os.Getenv("HOME")
	configFile = envOr("RGBCTL_CONFIG", filepath.Join(home, ".config/rgbctl/config.toml"))
	stateFile  = filepath.Join(home, ".local/state/rgbctl/state.json")
	cfg        Config
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "rgbctl: "+format+"\n", a...)
	os.Exit(1)
}

func loadConfig() {
	_, err := toml.DecodeFile(configFile, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		fail("no config at %s; start from one in examples/", configFile)
	}
	if err != nil {
		fail("%s: %v", configFile, err)
	}
	if len(cfg.Groups) == 0 {
		fail("%s: no [[group]] entries", configFile)
	}
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		if g.Label == "" {
			g.Label = g.Name
		}
		switch g.Backend {
		case "openrgb":
			if g.Name == "" || g.Device == "" {
				fail("%s: an openrgb [[group]] needs name and device", configFile)
			}
		case "via":
			if g.Name == "" || len(g.HIDIDs) == 0 {
				fail("%s: a via [[group]] needs name and hid_ids", configFile)
			}
			if g.Channel == 0 {
				g.Channel = 3 // qmk_rgb_matrix
			}
			if g.Effects == nil {
				g.Effects = map[string]uint8{"static": 1, "breathing": 2, "rainbow": 5}
			}
			if g.Speed == nil {
				s := uint8(96)
				g.Speed = &s
			}
		default:
			fail("%s: group %q: backend must be openrgb or via", configFile, g.Name)
		}
	}
	if cfg.OpenRGB.Host == "" {
		cfg.OpenRGB.Host = "127.0.0.1"
	}
	if cfg.OpenRGB.Port == 0 {
		cfg.OpenRGB.Port = 6742
	}
	// OpenRGB hardware mode per effect, first match wins.
	modes := map[string][]string{"breathing": {"Breathing"}, "rainbow": {"Rainbow Flow", "Rainbow", "Spectrum Cycle"}}
	for k, v := range cfg.OpenRGB.Modes {
		modes[k] = v
	}
	cfg.OpenRGB.Modes = modes
	if strings.HasPrefix(cfg.ThemeColors, "~/") {
		cfg.ThemeColors = filepath.Join(home, cfg.ThemeColors[2:])
	}
}

func groups(backend string) []Group {
	var out []Group
	for _, g := range cfg.Groups {
		if g.Backend == backend {
			out = append(out, g)
		}
	}
	return out
}

func loadState() State {
	s := State{On: true, Brightness: 100, Effect: "static", Color: "theme", Groups: map[string]bool{}}
	for _, g := range cfg.Groups {
		s.Groups[g.Name] = true
	}
	var saved State
	if raw, err := os.ReadFile(stateFile); err == nil && json.Unmarshal(raw, &saved) == nil {
		known := s.Groups
		s.On, s.Brightness, s.Effect, s.Color = saved.On, saved.Brightness, saved.Effect, saved.Color
		s.Groups = known
		for k, v := range saved.Groups {
			if _, ok := known[k]; ok {
				s.Groups[k] = v
			}
		}
	}
	return s
}

func saveState(s *State) {
	s.Resolved = resolvedColor(*s)
	s.GroupInfo = nil
	for _, g := range cfg.Groups {
		s.GroupInfo = append(s.GroupInfo, map[string]string{"name": g.Name, "label": g.Label, "backend": g.Backend})
	}
	raw, _ := json.MarshalIndent(s, "", "  ")
	os.MkdirAll(filepath.Dir(stateFile), 0o755)
	// Written in place (not renamed) so the bar widget's file watch keeps working.
	f, err := os.OpenFile(stateFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		fail("%v", err)
	}
	f.Write(raw)
	f.Close()
}

// themeColor is the `accent` of a theme colours file (TOML).
func themeColor() string {
	var colors map[string]any
	if cfg.ThemeColors == "" {
		return "#ffffff"
	}
	if _, err := toml.DecodeFile(cfg.ThemeColors, &colors); err != nil {
		return "#ffffff"
	}
	if a, ok := colors["accent"].(string); ok {
		return a
	}
	return "#ffffff"
}

func hexRGB(h string) RGB {
	h = strings.TrimPrefix(h, "#")
	v, _ := strconv.ParseUint(h, 16, 32)
	return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}
}

// vivid keeps a theme accent's hue and pushes saturation and value: LEDs
// wash out pastel colours.
func vivid(hex string) string {
	h, s, _ := rgbToHSV(hexRGB(hex))
	if s < 0.08 { // greys and whites stay white
		return "#ffffff"
	}
	c := hsvToRGB(h, math.Max(s, 0.6), 1)
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

func resolvedColor(s State) string {
	if s.Color == "theme" {
		return vivid(themeColor())
	}
	return s.Color
}

func rgbToHSV(c RGB) (h, s, v float64) {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	v = mx
	if mx == mn {
		return 0, 0, v
	}
	s = (mx - mn) / mx
	rc, gc, bc := (mx-r)/(mx-mn), (mx-g)/(mx-mn), (mx-b)/(mx-mn)
	switch mx {
	case r:
		h = bc - gc
	case g:
		h = 2 + rc - bc
	default:
		h = 4 + gc - rc
	}
	h = math.Mod(h/6, 1)
	if h < 0 {
		h++
	}
	return h, s, v
}

func hsvToRGB(h, s, v float64) RGB {
	i := math.Floor(h * 6)
	f := h*6 - i
	p, q, t := v*(1-s), v*(1-s*f), v*(1-s*(1-f))
	var r, g, b float64
	switch int(i) % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	return RGB{uint8(math.Round(r * 255)), uint8(math.Round(g * 255)), uint8(math.Round(b * 255))}
}

// connect waits, with wait set, for the server and for every configured
// device (the server answers before its detection finishes).
func connect(wait bool) (*orgbClient, []Device, error) {
	addr := net.JoinHostPort(cfg.OpenRGB.Host, strconv.Itoa(cfg.OpenRGB.Port))
	deadline := time.Now()
	if wait {
		deadline = deadline.Add(80 * time.Second)
	}
	for {
		c, err := dialOpenRGB(addr)
		if err == nil {
			devs, err := c.devices()
			if err == nil && (!wait || allPresent(devs)) {
				return c, devs, nil
			}
			c.close()
			if err != nil && time.Now().After(deadline) {
				return nil, nil, err
			}
		} else if time.Now().After(deadline) {
			return nil, nil, err
		}
		if time.Now().After(deadline) {
			return nil, nil, errors.New("not every configured device appeared")
		}
		time.Sleep(2 * time.Second)
	}
}

func allPresent(devs []Device) bool {
	for _, g := range groups("openrgb") {
		found := false
		for _, d := range devs {
			found = found || strings.Contains(d.Name, g.Device)
		}
		if !found {
			return false
		}
	}
	return true
}

func applyOpenRGB(s State, wait bool) error {
	if len(groups("openrgb")) == 0 {
		return nil
	}
	c, devs, err := connect(wait)
	if err != nil {
		return err
	}
	defer c.close()
	k := float64(max(0, min(100, s.Brightness))) / 100
	base := hexRGB(resolvedColor(s))
	color := RGB{uint8(math.Round(float64(base.R) * k)), uint8(math.Round(float64(base.G) * k)), uint8(math.Round(float64(base.B) * k))}
	effect := s.Effect
	if !s.On {
		effect = "off"
	}
	type member struct {
		zone *int
		on   bool
	}
	byDevice := map[uint32][]member{}
	var order []Device
	for _, g := range groups("openrgb") {
		for _, d := range devs {
			if strings.Contains(d.Name, g.Device) {
				if _, seen := byDevice[d.ID]; !seen {
					order = append(order, d)
				}
				byDevice[d.ID] = append(byDevice[d.ID], member{g.Zone, s.Groups[g.Name]})
			}
		}
	}
	for _, d := range order {
		members := byDevice[d.ID]
		allLit := true
		for _, m := range members {
			allLit = allLit && m.on
		}
		// Hardware effects run per device; one strip off on a shared
		// controller falls back to a static colour on the other.
		if mode := findMode(d, cfg.OpenRGB.Modes[effect]...); mode != nil && allLit {
			m := *mode
			if m.ColorMode == colorModeSpecific {
				m.Colors = []RGB{color}
			}
			if m.Flags&flagHasSpeed != 0 {
				m.Speed = (m.SpeedMin + m.SpeedMax) / 2
			}
			if err := c.setMode(d, m); err != nil {
				return err
			}
			continue
		}
		if direct := findMode(d, "Direct"); direct != nil {
			if err := c.setMode(d, *direct); err != nil {
				return err
			}
		}
		for _, m := range members {
			col := RGB{}
			if m.on && effect != "off" {
				col = color
			}
			var err error
			if m.zone == nil {
				err = c.setDeviceColor(d, col)
			} else if *m.zone < len(d.Zones) {
				err = c.setZoneColor(d, d.Zones[*m.zone], col)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// apply applies to everything; one backend failing doesn't stop the others.
func apply(s State, wait bool) {
	err := applyOpenRGB(s, wait)
	for _, g := range groups("via") {
		if _, e := applyVIA(g, s); e != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", g.Name, e)
		}
	}
	if err != nil {
		fail("openrgb: %v", err)
	}
}

func parseBool(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func setValues(s *State, args []string) {
	for i := 0; i+1 < len(args); i += 2 {
		key, value := args[i], args[i+1]
		switch {
		case key == "on":
			s.On = parseBool(value)
		case key == "brightness":
			n, err := strconv.Atoi(value)
			if err != nil {
				fail("invalid setting: %s %s", key, value)
			}
			s.Brightness = max(0, min(100, n))
		case key == "effect" && contains(effects, value):
			s.Effect = value
		case key == "color" && (value == "theme" || (len(value) == 7 && value[0] == '#')):
			s.Color = strings.ToLower(value)
		case strings.HasPrefix(key, "group."):
			if _, ok := s.Groups[key[6:]]; !ok {
				fail("invalid setting: %s %s", key, value)
			}
			s.Groups[key[6:]] = parseBool(value)
		default:
			fail("invalid setting: %s %s", key, value)
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func identify(device string) {
	c, devs, err := connect(true)
	if err != nil {
		fail("openrgb: %v", err)
	}
	var dev *Device
	for i := range devs {
		if strings.Contains(devs[i].Name, device) {
			dev = &devs[i]
			break
		}
	}
	if dev == nil {
		var names []string
		for _, d := range devs {
			names = append(names, d.Name)
		}
		fail("no OpenRGB device matching %q: %s", device, strings.Join(names, ", "))
	}
	if direct := findMode(*dev, "Direct"); direct != nil {
		c.setMode(*dev, *direct)
	}
	for _, z := range dev.Zones {
		for _, other := range dev.Zones {
			c.setZoneColor(*dev, other, RGB{})
		}
		c.setZoneColor(*dev, z, RGB{255, 0, 0})
		fmt.Printf("zone %d (%s) red for 6s...\n", z.ID, z.Name)
		time.Sleep(6 * time.Second)
	}
	c.close()
	apply(loadState(), false)
}

func viaGroup(name string) Group {
	for _, g := range groups("via") {
		if g.Name == name {
			return g
		}
	}
	fail("no via group %q", name)
	return Group{}
}

func main() {
	loadConfig()
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	s := loadState()
	switch {
	case cmd == "apply":
		saveState(&s)
		apply(s, contains(args, "--wait"))
	case cmd == "set" && len(args) >= 3 && len(args)%2 == 1:
		setValues(&s, args[1:])
		saveState(&s)
		apply(s, false)
	case cmd == "toggle":
		s.On = !s.On
		saveState(&s)
		apply(s, false)
	case cmd == "sync" && len(args) <= 2:
		targets := groups("via")
		if len(args) == 2 {
			targets = []Group{viaGroup(args[1])}
		}
		for _, g := range targets {
			ok, err := applyVIA(g, s)
			if err != nil {
				fail("%s: %v", g.Name, err)
			}
			if !ok {
				fail("%s: not reachable", g.Name)
			}
		}
	case cmd == "link" && len(args) == 2:
		fmt.Println(viaLink(viaGroup(args[1])))
	case cmd == "state":
		saveState(&s)
		raw, _ := json.Marshal(s)
		fmt.Println(string(raw))
	case cmd == "identify" && len(args) == 2:
		identify(args[1])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}
