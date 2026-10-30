package main

// VIA (QMK) lighting over raw hidraw: brightness, effect, speed and colour on
// the RGB matrix channel.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	hidiocgrdescsize = 0x80044801
	hidiocgrdesc     = 0x90044802
)

var viaUsagePage = []byte{0x06, 0x60, 0xff, 0x09, 0x61} // usage page 0xFF60, usage 0x61

func hidrawDesc(node string) ([]byte, error) {
	f, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var size int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), hidiocgrdescsize, uintptr(unsafe.Pointer(&size))); e != 0 {
		return nil, e
	}
	buf := make([]byte, 4+4096)
	binary.LittleEndian.PutUint32(buf, uint32(size))
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), hidiocgrdesc, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return nil, e
	}
	return buf[4 : 4+size], nil
}

// viaHidraw finds the keyboard's VIA node and the rank of the HID id it
// matched (0 = the first, the cable).
func viaHidraw(g Group) (string, int) {
	type hit struct {
		rank int
		node string
	}
	var hits []hit
	uevents, _ := filepath.Glob("/sys/class/hidraw/hidraw*/device/uevent")
	for _, u := range uevents {
		raw, err := os.ReadFile(u)
		if err != nil {
			continue
		}
		for rank, id := range g.HIDIDs {
			if strings.Contains(string(raw), "HID_ID="+id) {
				hits = append(hits, hit{rank, "/dev/" + strings.Split(u, "/")[4]})
				break
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].rank < hits[j].rank })
	for _, h := range hits {
		if desc, err := hidrawDesc(h.node); err == nil && bytes.HasPrefix(desc, viaUsagePage) {
			return h.node, h.rank
		}
	}
	return "", -1
}

type via struct {
	fd      int
	channel byte
}

func openVIA(node string, channel int) (*via, error) {
	fd, err := syscall.Open(node, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return &via{fd, byte(channel)}, nil
}

func (v *via) close() { syscall.Close(v.fd) }

type pollFd struct {
	fd      int32
	events  int16
	revents int16
}

func (v *via) xfer(timeout time.Duration, cmd ...byte) ([]byte, error) {
	buf := make([]byte, 64)
	for {
		if n, err := syscall.Read(v.fd, buf); n <= 0 || err != nil {
			break
		}
	}
	pkt := make([]byte, 33)
	copy(pkt[1:], cmd)
	if _, err := syscall.Write(v.fd, pkt); err != nil {
		return nil, err
	}
	fds := []pollFd{{fd: int32(v.fd), events: 1}}
	n, _, _ := syscall.Syscall(syscall.SYS_POLL, uintptr(unsafe.Pointer(&fds[0])), 1, uintptr(timeout.Milliseconds()))
	if int(n) <= 0 {
		return nil, errors.New("keyboard did not answer")
	}
	r, err := syscall.Read(v.fd, buf)
	if err != nil {
		return nil, err
	}
	return buf[:r], nil
}

func (v *via) get(id byte) ([]byte, error) {
	r, err := v.xfer(time.Second, 0x08, v.channel, id)
	if err != nil || len(r) < 5 {
		return nil, err
	}
	return r[3:5], nil
}

func (v *via) set(id byte, data ...byte) error {
	_, err := v.xfer(time.Second, append([]byte{0x07, v.channel, id}, data...)...)
	return err
}

func (v *via) save() error {
	_, err := v.xfer(time.Second, 0x09, v.channel)
	return err
}

// viaLink is "wired" (the first HID id), "wireless" (a later one, such as a
// dongle) or "absent" (not found, or not answering VIA).
func viaLink(g Group) string {
	node, rank := viaHidraw(g)
	if node == "" {
		return "absent"
	}
	v, err := openVIA(node, g.Channel)
	if err != nil {
		return "absent"
	}
	defer v.close()
	// VIA protocol version. Through a dongle the first request may only wake
	// the keyboard up, so allow a second, slower try.
	for _, t := range []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond} {
		if _, err := v.xfer(t, 0x01); err == nil {
			if rank == 0 {
				return "wired"
			}
			return "wireless"
		}
	}
	return "absent"
}

// applyVIA pushes the state to one keyboard; false when it is not reachable.
func applyVIA(g Group, s State) (bool, error) {
	node, _ := viaHidraw(g)
	if node == "" {
		return false, nil
	}
	h, sat, val := rgbToHSV(hexRGB(resolvedColor(s)))
	lit := s.On && s.Groups[g.Name]
	bright := byte(0)
	if lit {
		bright = byte(math.Round(255 * val * float64(s.Brightness) / 100))
	}
	want := []struct {
		id   byte
		data []byte
	}{
		{1, []byte{bright}},
		{2, []byte{g.Effects[s.Effect]}},
		{3, []byte{*g.Speed}},
		{4, []byte{byte(math.Round(h * 255)), byte(math.Round(sat * 255))}},
	}
	v, err := openVIA(node, g.Channel)
	if err != nil {
		return false, err
	}
	defer v.close()
	for _, w := range want {
		cur, err := v.get(w.id)
		if err != nil {
			return false, err
		}
		if bytes.Equal(cur[:len(w.data)], w.data) {
			continue
		}
		if err := v.set(w.id, w.data...); err != nil {
			return false, err
		}
		// Some VIA firmwares only persist a save when exactly one value changed
		// since the last one (it compares the mask with ==), so save after
		// every value. EEPROM writes still only happen for values that changed.
		if err := v.save(); err != nil {
			return false, err
		}
	}
	return true, nil
}
