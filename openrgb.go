package main

// A minimal client for the OpenRGB SDK protocol (version 4): list devices,
// set a mode, set zone or device colours.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const protocolVersion = 4

const (
	pktControllerCount = 0
	pktControllerData  = 1
	pktProtocolVersion = 40
	pktSetClientName   = 50
	pktUpdateLEDs      = 1050
	pktUpdateZoneLEDs  = 1051
	pktUpdateMode      = 1101
)

const (
	flagHasSpeed       = 1 << 0
	flagHasDirectionLR = 1 << 1
	flagHasDirectionUD = 1 << 2
	flagHasDirectionHV = 1 << 3
	flagHasBrightness  = 1 << 4
	colorModeSpecific  = 2
)

type RGB struct{ R, G, B uint8 }

type Mode struct {
	ID                           int32
	Name                         string
	Value                        int32
	Flags                        uint32
	SpeedMin, SpeedMax           uint32
	BrightnessMin, BrightnessMax uint32
	ColorsMin, ColorsMax         uint32
	Speed, Brightness, Direction uint32
	ColorMode                    uint32
	Colors                       []RGB
}

type Zone struct {
	ID   int32
	Name string
	LEDs uint32
}

type Device struct {
	ID    uint32
	Name  string
	Modes []Mode
	Zones []Zone
	LEDs  int
}

type orgbClient struct {
	conn    net.Conn
	version uint32
}

func dialOpenRGB(addr string) (*orgbClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	c := &orgbClient{conn: conn, version: protocolVersion}
	if err := c.send(0, pktProtocolVersion, u32(protocolVersion)); err != nil {
		conn.Close()
		return nil, err
	}
	if reply, err := c.recv(pktProtocolVersion); err == nil && len(reply) >= 4 {
		c.version = min(binary.LittleEndian.Uint32(reply), protocolVersion)
	}
	if err := c.send(0, pktSetClientName, []byte("rgbctl\x00")); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *orgbClient) close() { c.conn.Close() }

func (c *orgbClient) send(dev, pkt uint32, data []byte) error {
	var b bytes.Buffer
	b.WriteString("ORGB")
	binary.Write(&b, binary.LittleEndian, [3]uint32{dev, pkt, uint32(len(data))})
	b.Write(data)
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write(b.Bytes())
	return err
}

// recv reads packets until one of type pkt arrives (the server also sends
// unsolicited "device list updated" packets).
func (c *orgbClient) recv(pkt uint32) ([]byte, error) {
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var hdr [16]byte
		if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
			return nil, err
		}
		if string(hdr[:4]) != "ORGB" {
			return nil, errors.New("openrgb: bad packet header")
		}
		size := binary.LittleEndian.Uint32(hdr[12:])
		data := make([]byte, size)
		if _, err := io.ReadFull(c.conn, data); err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(hdr[8:]) == pkt {
			return data, nil
		}
	}
}

func (c *orgbClient) devices() ([]Device, error) {
	if err := c.send(0, pktControllerCount, nil); err != nil {
		return nil, err
	}
	raw, err := c.recv(pktControllerCount)
	if err != nil || len(raw) < 4 {
		return nil, fmt.Errorf("openrgb: controller count: %v", err)
	}
	var devs []Device
	for i := uint32(0); i < binary.LittleEndian.Uint32(raw); i++ {
		if err := c.send(i, pktControllerData, u32(c.version)); err != nil {
			return nil, err
		}
		data, err := c.recv(pktControllerData)
		if err != nil {
			return nil, err
		}
		d, err := parseController(data, c.version)
		if err != nil {
			return nil, fmt.Errorf("openrgb: device %d: %w", i, err)
		}
		d.ID = i
		devs = append(devs, d)
	}
	return devs, nil
}

func (c *orgbClient) setMode(dev Device, m Mode) error {
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	w(m.ID)
	writeString(&b, m.Name)
	w(m.Value)
	w(m.Flags)
	// Fields a mode doesn't support are sent as 0, as the server expects.
	speedMin, speedMax, speed := m.SpeedMin, m.SpeedMax, m.Speed
	if m.Flags&flagHasSpeed == 0 {
		speedMin, speedMax, speed = 0, 0, 0
	}
	bMin, bMax, bright := m.BrightnessMin, m.BrightnessMax, m.Brightness
	if m.Flags&flagHasBrightness == 0 {
		bMin, bMax, bright = 0, 0, 0
	}
	direction := m.Direction
	if m.Flags&(flagHasDirectionLR|flagHasDirectionUD|flagHasDirectionHV) == 0 {
		direction = 0
	}
	cMin, cMax := m.ColorsMin, m.ColorsMax
	if len(m.Colors) == 0 {
		cMin, cMax = 0, 0
	}
	w(speedMin)
	w(speedMax)
	if c.version >= 3 {
		w(bMin)
		w(bMax)
	}
	w(cMin)
	w(cMax)
	w(speed)
	if c.version >= 3 {
		w(bright)
	}
	w(direction)
	w(m.ColorMode)
	w(uint16(len(m.Colors)))
	for _, col := range m.Colors {
		b.Write([]byte{col.R, col.G, col.B, 0})
	}
	return c.send(dev.ID, pktUpdateMode, append(u32(uint32(b.Len()+4)), b.Bytes()...))
}

func (c *orgbClient) setZoneColor(dev Device, z Zone, col RGB) error {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, z.ID)
	binary.Write(&b, binary.LittleEndian, uint16(z.LEDs))
	for range z.LEDs {
		b.Write([]byte{col.R, col.G, col.B, 0})
	}
	return c.send(dev.ID, pktUpdateZoneLEDs, append(u32(uint32(b.Len()+4)), b.Bytes()...))
}

func (c *orgbClient) setDeviceColor(dev Device, col RGB) error {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint16(dev.LEDs))
	for range dev.LEDs {
		b.Write([]byte{col.R, col.G, col.B, 0})
	}
	return c.send(dev.ID, pktUpdateLEDs, append(u32(uint32(b.Len()+4)), b.Bytes()...))
}

func findMode(dev Device, names ...string) *Mode {
	for _, want := range names {
		for i := range dev.Modes {
			if strings.EqualFold(dev.Modes[i].Name, want) {
				return &dev.Modes[i]
			}
		}
	}
	return nil
}

func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func writeString(b *bytes.Buffer, s string) {
	binary.Write(b, binary.LittleEndian, uint16(len(s)+1))
	b.WriteString(s)
	b.WriteByte(0)
}

// reader walks a controller data packet.
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || len(r.b) < n {
		r.err = errors.New("short controller data")
		return make([]byte, n)
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u16() uint16 { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *reader) u32() uint32 { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *reader) str() string { return strings.TrimRight(string(r.take(int(r.u16()))), "\x00") }

func parseController(data []byte, version uint32) (Device, error) {
	r := &reader{b: data}
	var d Device
	r.u32() // size
	r.u32() // device type
	d.Name = r.str()
	if version >= 1 {
		r.str() // vendor
	}
	for range 4 { // description, version, serial, location
		r.str()
	}
	numModes := int(r.u16())
	r.u32() // active mode
	for i := range numModes {
		m := Mode{ID: int32(i), Name: r.str(), Value: int32(r.u32()), Flags: r.u32()}
		m.SpeedMin, m.SpeedMax = r.u32(), r.u32()
		if version >= 3 {
			m.BrightnessMin, m.BrightnessMax = r.u32(), r.u32()
		}
		m.ColorsMin, m.ColorsMax = r.u32(), r.u32()
		m.Speed = r.u32()
		if version >= 3 {
			m.Brightness = r.u32()
		}
		m.Direction, m.ColorMode = r.u32(), r.u32()
		for range r.u16() {
			c := r.take(4)
			m.Colors = append(m.Colors, RGB{c[0], c[1], c[2]})
		}
		d.Modes = append(d.Modes, m)
	}
	for i := range int(r.u16()) {
		z := Zone{ID: int32(i), Name: r.str()}
		r.u32() // type
		r.u32() // leds min
		r.u32() // leds max
		z.LEDs = r.u32()
		if matrix := r.u16(); matrix > 0 {
			r.take(int(matrix))
		}
		if version >= 4 {
			for range r.u16() { // segments
				r.str()
				r.take(12)
			}
		}
		d.Zones = append(d.Zones, z)
	}
	d.LEDs = int(r.u16())
	return d, r.err
}
