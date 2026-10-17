//go:build linux

package hwenc

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// The tests stand in for ffmpeg and gst-launch-1.0 with this test binary:
// run with fakeEnv set, it reads NV12 frames on stdin and writes framed
// access units on stdout as the real tools do.
const fakeEnv = "ROBIN_HWENC_FAKE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		fake(mode, os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeRoute runs the fake tool in mode, "framing:behaviour", where framing
// is multipart or flv and behaviour one of:
//
//	normal  one access unit per frame
//	hang    answer the first frame, then nothing
//	die     exit with a message on stderr after the first frame
//	lag     hold each access unit back until the next frame arrives
//	high    as normal, with a High profile SPS
//	spawn   as normal, after starting a grandchild in its process group
//	        whose pid goes to the file named by ROBIN_HWENC_FAKE_PIDFILE
//	parent  not a tool: open an Encoder on a normal fake, encode a frame,
//	        print the tool's pid and wait to be killed
func fakeRoute(mode string, env ...string) *route {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	f := newMultipartReader
	if strings.HasPrefix(mode, "flv:") {
		f = newFLVReader
	}
	return &route{
		name: "fake " + mode, path: exe, framing: f,
		env: append([]string{fakeEnv + "=" + mode}, env...),
		args: func(c config) []string {
			return []string{strconv.Itoa(c.width), strconv.Itoa(c.height), strconv.Itoa(c.bitrate)}
		},
	}
}

var (
	fakeSPS     = []byte{0x67, 0x42, 0xc0, 0x1f, 0xda, 0x01, 0x40, 0x16, 0xec, 0x04, 0x40}
	fakeHighSPS = []byte{0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb, 0x01}
	fakePPS     = []byte{0x68, 0xce, 0x3c, 0x80}
)

// fakeSlice is the slice NAL unit for frame n, carrying n and the frame's
// first luma byte, so a test can match access units to frames.
func fakeSlice(n int, luma byte, size int) []byte {
	typ := byte(0x41)
	if n == 0 {
		typ = 0x65
	}
	s := []byte{typ, 0x9a, byte(n >> 8), byte(n), luma}
	for len(s) < size {
		s = append(s, 0x55)
	}
	return s
}

func fake(mode string, args []string) {
	framing, behaviour, _ := strings.Cut(mode, ":")
	if behaviour == "parent" {
		fakeParent()
		return
	}
	w, _ := strconv.Atoi(args[0])
	h, _ := strconv.Atoi(args[1])
	// ROBIN_HWENC_FAKE_AU sets the size of every slice, to load the budget.
	size, _ := strconv.Atoi(os.Getenv("ROBIN_HWENC_FAKE_AU"))
	if behaviour == "spawn" {
		c := exec.Command("sleep", "300")
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.WriteFile(os.Getenv("ROBIN_HWENC_FAKE_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
	}
	sps := fakeSPS
	if behaviour == "high" {
		sps = fakeHighSPS
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	frame := make([]byte, w*h*3/2)
	var held []byte
	for n := 0; ; n++ {
		if _, err := io.ReadFull(in, frame); err != nil {
			return
		}
		nals := [][]byte{fakeSlice(n, frame[0], size)}
		if n == 0 {
			nals = [][]byte{sps, fakePPS, nals[0]}
		}
		au := writeAU(nil, framing, n, nals)
		switch {
		case behaviour == "hang" && n > 0:
			continue
		case behaviour == "die":
			fmt.Fprintln(os.Stderr, "fake: exploded")
			os.Exit(3)
		case behaviour == "lag":
			au, held = held, au
		}
		os.Stdout.Write(au)
	}
}

// writeAU appends access unit n in the framing a real tool uses. The FLV
// form keeps the SPS and PPS in the sequence header only, as ffmpeg does.
func writeAU(b []byte, framing string, n int, nals [][]byte) []byte {
	if framing == "multipart" {
		var au []byte
		for _, nal := range nals {
			au = append(append(au, startCode...), nal...)
		}
		b = fmt.Appendf(b, "--robin\r\nContent-Type: video/x-h264\r\nContent-Length: %d\r\n\r\n", len(au))
		return append(append(b, au...), "\r\n"...)
	}
	if n == 0 {
		b = append(b, flvHeader...)
		b = flvTag(b, 18, []byte("\x02\x00\x0aonMetaData"))
		b = flvSequence(b, nals[0], nals[1])
		nals = nals[2:]
	}
	data := []byte{0x27, flvAVCNALU, 0, 0, 0}
	if n == 0 {
		data[0] = 0x17
	}
	for _, nal := range nals {
		data = binary.BigEndian.AppendUint32(data, uint32(len(nal)))
		data = append(data, nal...)
	}
	return flvTag(b, flvTagVideo, data)
}

// flvHeader is the file header and the first PreviousTagSize.
const flvHeader = "FLV\x01\x01\x00\x00\x00\x09\x00\x00\x00\x00"

// flvSequence appends the AVC sequence header tag for one SPS and one PPS.
func flvSequence(b, sps, pps []byte) []byte {
	rec := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1}
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(sps)))
	rec = append(append(rec, sps...), 1)
	rec = binary.BigEndian.AppendUint16(rec, uint16(len(pps)))
	rec = append(rec, pps...)
	return flvTag(b, flvTagVideo, append([]byte{0x17, flvAVCSequence, 0, 0, 0}, rec...))
}

func flvTag(b []byte, typ byte, data []byte) []byte {
	n := len(data)
	b = append(b, typ, byte(n>>16), byte(n>>8), byte(n), 0, 0, 0, 0, 0, 0, 0)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, uint32(flvTagHeaderLen+n))
}

func fakeParent() {
	e := newEncoder(fakeRoute("multipart:normal"), 64, 64, 30, 1_000_000)
	if _, err := e.Encode(yuv.NewI420(64, 64), 0, true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(e.proc.cmd.Process.Pid)
	time.Sleep(time.Minute)
}

// waitGone waits for pid to exit, counting a zombie as gone: once the
// process is dead its reaping is the parent's business.
func waitGone(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			if i := strings.LastIndexByte(string(stat), ')'); i > 0 && strings.HasPrefix(string(stat[i+1:]), " Z") {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %d still running after %v", pid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
