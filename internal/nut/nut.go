// Package nut is a minimal streaming demuxer for FFmpeg's NUT container.
//
// Why NUT: the host reads encoded video from an ffmpeg child process. Raw
// Annex-B output has no frame boundaries (you only learn a frame ended when the
// next one starts, costing a full frame of latency), and MP4/Matroska muxers
// hold each packet until the following one arrives. FFmpeg's NUT muxer writes
// every packet immediately, prefixed with its exact size, so a frame can be
// forwarded the instant its last byte leaves the encoder.
//
// Only the subset of NUT produced by libavformat's muxer (version 3) is
// supported: main/stream/info/index/syncpoint packets and frames.
package nut

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const (
	mainStartcode      = 0x4E4D7A561F5F04AD
	streamStartcode    = 0x4E5311405BF2F9DB
	syncpointStartcode = 0x4E4BE4ADEECA4569
	indexStartcode     = 0x4E58DD672F23E64E
	infoStartcode      = 0x4E49AB68B596BA78
)

const idString = "nut/multimedia container\x00"

const (
	flagKey       = 1
	flagEOR       = 2
	flagCodedPTS  = 8
	flagStreamID  = 16
	flagSizeMSB   = 32
	flagChecksum  = 64
	flagReserved  = 128
	flagSMData    = 256
	flagHeaderIdx = 1024
	flagMatchTime = 2048
	flagCoded     = 4096
	flagInvalid   = 8192
)

// Stream classes.
const (
	ClassVideo    = 0
	ClassAudio    = 1
	ClassSubtitle = 2
	ClassData     = 3
)

const maxPacketHeader = 1 << 20 // sanity limit for header packets

// Rational is a time base.
type Rational struct{ Num, Den int64 }

// Stream describes one elementary stream.
type Stream struct {
	ID             int
	Class          int
	FourCC         string
	TimeBase       Rational
	Extradata      []byte
	Width, Height  int
	SampleRate     int
	Channels       int
	msbPtsShift    uint
	maxPtsDistance uint64
	lastPts        int64
	timeBaseID     int
}

// Packet is one demuxed frame.
type Packet struct {
	Stream int
	Pts    int64 // in the stream's time base
	Key    bool
	Data   []byte
}

// PtsMicros converts the packet timestamp to microseconds.
func (p *Packet) PtsMicros(s *Stream) int64 {
	if s.TimeBase.Den == 0 {
		return 0
	}
	return p.Pts * 1_000_000 * s.TimeBase.Num / s.TimeBase.Den
}

type frameCode struct {
	flags         uint64
	streamID      int
	sizeMul       uint64
	sizeLSB       uint64
	ptsDelta      int64
	reservedCount uint64
	headerIdx     int
}

// Demuxer reads packets from a NUT byte stream.
type Demuxer struct {
	r          *bufio.Reader
	started    bool
	version    uint64
	streams    []*Stream
	timeBases  []Rational
	frameCodes [256]frameCode
	headers    [][]byte
	maxSize    int
	gotMain    bool
}

// NewDemuxer wraps r. maxFrame bounds the size of a single frame.
func NewDemuxer(r io.Reader, maxFrame int) *Demuxer {
	return &Demuxer{r: bufio.NewReaderSize(r, 256<<10), maxSize: maxFrame}
}

// Streams returns the streams declared so far (complete once the first packet is returned).
func (d *Demuxer) Streams() []*Stream { return d.streams }

var ErrFormat = errors.New("nut: invalid stream")

// ReadPacket returns the next frame, transparently consuming header packets.
func (d *Demuxer) ReadPacket() (*Packet, error) {
	if !d.started {
		id := make([]byte, len(idString))
		if _, err := io.ReadFull(d.r, id); err != nil {
			return nil, err
		}
		if string(id) != idString {
			return nil, fmt.Errorf("%w: bad magic", ErrFormat)
		}
		d.started = true
	}
	for {
		b, err := d.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 'N' {
			rest := make([]byte, 7)
			if _, err := io.ReadFull(d.r, rest); err != nil {
				return nil, err
			}
			sc := uint64('N')<<56 | uint64(rest[0])<<48 | uint64(rest[1])<<40 | uint64(rest[2])<<32 |
				uint64(rest[3])<<24 | uint64(rest[4])<<16 | uint64(rest[5])<<8 | uint64(rest[6])
			payload, err := d.readPacketPayload()
			if err != nil {
				return nil, err
			}
			switch sc {
			case mainStartcode:
				if err := d.parseMain(payload); err != nil {
					return nil, err
				}
			case streamStartcode:
				if err := d.parseStream(payload); err != nil {
					return nil, err
				}
			case syncpointStartcode:
				if err := d.parseSyncpoint(payload); err != nil {
					return nil, err
				}
			case infoStartcode, indexStartcode:
				// ignored
			default:
				return nil, fmt.Errorf("%w: unknown startcode %016x", ErrFormat, sc)
			}
			continue
		}
		if !d.gotMain {
			return nil, fmt.Errorf("%w: frame before main header", ErrFormat)
		}
		return d.readFrame(b)
	}
}

// readPacketPayload reads forward_ptr (+ optional header checksum) and the payload
// (which includes the trailing 4-byte checksum, stripped here).
func (d *Demuxer) readPacketPayload() ([]byte, error) {
	fwd, err := readV(d.r)
	if err != nil {
		return nil, err
	}
	if fwd > 4096 {
		if _, err := io.ReadFull(d.r, make([]byte, 4)); err != nil {
			return nil, err
		}
	}
	if fwd < 4 || fwd > maxPacketHeader {
		return nil, fmt.Errorf("%w: bad forward_ptr %d", ErrFormat, fwd)
	}
	buf := make([]byte, fwd)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return nil, err
	}
	return buf[:fwd-4], nil
}

func (d *Demuxer) parseMain(p []byte) error {
	r := bytes.NewReader(p)
	var err error
	if d.version, err = readV(r); err != nil {
		return err
	}
	if d.version < 2 || d.version > 4 {
		return fmt.Errorf("%w: unsupported version %d", ErrFormat, d.version)
	}
	if d.version > 3 {
		if _, err = readV(r); err != nil { // minor version
			return err
		}
	}
	nStreams, err := readV(r)
	if err != nil || nStreams == 0 || nStreams > 256 {
		return fmt.Errorf("%w: stream count", ErrFormat)
	}
	if _, err = readV(r); err != nil { // max_distance
		return err
	}
	ntb, err := readV(r)
	if err != nil || ntb == 0 || ntb > 256 {
		return fmt.Errorf("%w: time base count", ErrFormat)
	}
	d.timeBases = make([]Rational, ntb)
	for i := range d.timeBases {
		num, err1 := readV(r)
		den, err2 := readV(r)
		if err1 != nil || err2 != nil || num == 0 || den == 0 {
			return fmt.Errorf("%w: time base", ErrFormat)
		}
		d.timeBases[i] = Rational{int64(num), int64(den)}
	}
	var (
		tmpPts     int64
		tmpMul     uint64 = 1
		tmpStream  uint64
		tmpHeadIdx uint64
	)
	for i := 0; i < 256; {
		tmpFlags, err := readV(r)
		if err != nil {
			return err
		}
		tmpFields, err := readV(r)
		if err != nil {
			return err
		}
		if tmpFields > 0 {
			if tmpPts, err = readS(r); err != nil {
				return err
			}
		}
		if tmpFields > 1 {
			if tmpMul, err = readV(r); err != nil {
				return err
			}
		}
		if tmpFields > 2 {
			if tmpStream, err = readV(r); err != nil {
				return err
			}
		}
		var tmpSize, tmpRes uint64
		if tmpFields > 3 {
			if tmpSize, err = readV(r); err != nil {
				return err
			}
		}
		if tmpFields > 4 {
			if tmpRes, err = readV(r); err != nil {
				return err
			}
		}
		var count uint64
		if tmpFields > 5 {
			if count, err = readV(r); err != nil {
				return err
			}
		} else {
			count = tmpMul - tmpSize
		}
		if tmpFields > 6 {
			if _, err = readS(r); err != nil {
				return err
			}
		}
		if tmpFields > 7 {
			if tmpHeadIdx, err = readV(r); err != nil {
				return err
			}
		}
		for f := tmpFields; f > 8; f-- {
			if _, err = readV(r); err != nil {
				return err
			}
		}
		lim := uint64(256 - i)
		if i <= 'N' {
			lim--
		}
		if count == 0 || count > lim || tmpStream >= nStreams {
			return fmt.Errorf("%w: frame code table", ErrFormat)
		}
		for j := uint64(0); j < count; j, i = j+1, i+1 {
			if i == 'N' {
				d.frameCodes[i] = frameCode{flags: flagInvalid}
				j--
				continue
			}
			d.frameCodes[i] = frameCode{
				flags:         tmpFlags,
				ptsDelta:      tmpPts,
				streamID:      int(tmpStream),
				sizeMul:       tmpMul,
				sizeLSB:       tmpSize + j,
				reservedCount: tmpRes,
				headerIdx:     int(tmpHeadIdx),
			}
		}
	}
	d.headers = [][]byte{nil}
	if r.Len() > 0 {
		hc, err := readV(r)
		if err != nil || hc >= 128 {
			return fmt.Errorf("%w: header count", ErrFormat)
		}
		for i := uint64(0); i < hc; i++ {
			l, err := readV(r)
			if err != nil || l == 0 || l > 255 {
				return fmt.Errorf("%w: elision header", ErrFormat)
			}
			h := make([]byte, l)
			if _, err := io.ReadFull(r, h); err != nil {
				return err
			}
			d.headers = append(d.headers, h)
		}
	}
	d.streams = make([]*Stream, nStreams)
	d.gotMain = true
	return nil
}

func (d *Demuxer) parseStream(p []byte) error {
	if !d.gotMain {
		return fmt.Errorf("%w: stream header before main header", ErrFormat)
	}
	r := bytes.NewReader(p)
	id, err := readV(r)
	if err != nil || id >= uint64(len(d.streams)) {
		return fmt.Errorf("%w: stream id", ErrFormat)
	}
	s := &Stream{ID: int(id)}
	class, err := readV(r)
	if err != nil {
		return err
	}
	s.Class = int(class)
	fl, err := readV(r)
	if err != nil || fl > 16 {
		return fmt.Errorf("%w: fourcc", ErrFormat)
	}
	fcc := make([]byte, fl)
	if _, err := io.ReadFull(r, fcc); err != nil {
		return err
	}
	s.FourCC = string(fcc)
	tbID, err := readV(r)
	if err != nil || tbID >= uint64(len(d.timeBases)) {
		return fmt.Errorf("%w: time base id", ErrFormat)
	}
	s.timeBaseID = int(tbID)
	s.TimeBase = d.timeBases[tbID]
	shift, err := readV(r)
	if err != nil || shift >= 16 {
		return fmt.Errorf("%w: msb_pts_shift", ErrFormat)
	}
	s.msbPtsShift = uint(shift)
	if s.maxPtsDistance, err = readV(r); err != nil {
		return err
	}
	if _, err = readV(r); err != nil { // decode_delay
		return err
	}
	if _, err = readV(r); err != nil { // stream flags
		return err
	}
	exLen, err := readV(r)
	if err != nil || exLen > 1<<20 {
		return fmt.Errorf("%w: extradata", ErrFormat)
	}
	s.Extradata = make([]byte, exLen)
	if _, err := io.ReadFull(r, s.Extradata); err != nil {
		return err
	}
	switch s.Class {
	case ClassVideo:
		w, err1 := readV(r)
		h, err2 := readV(r)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("%w: video dims", ErrFormat)
		}
		s.Width, s.Height = int(w), int(h)
	case ClassAudio:
		sr, err1 := readV(r)
		_, err2 := readV(r) // samplerate_den
		ch, err3 := readV(r)
		if err1 != nil || err2 != nil || err3 != nil {
			return fmt.Errorf("%w: audio params", ErrFormat)
		}
		s.SampleRate, s.Channels = int(sr), int(ch)
	}
	d.streams[id] = s
	return nil
}

func (d *Demuxer) parseSyncpoint(p []byte) error {
	r := bytes.NewReader(p)
	tt, err := readV(r)
	if err != nil {
		return err
	}
	n := uint64(len(d.timeBases))
	tb := d.timeBases[tt%n]
	ts := int64(tt / n)
	for _, s := range d.streams {
		if s == nil {
			continue
		}
		// rescale ts from tb to the stream time base, rounding down.
		num := tb.Num * s.TimeBase.Den
		den := tb.Den * s.TimeBase.Num
		s.lastPts = floorDiv(ts*num, den)
	}
	return nil
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func (d *Demuxer) readFrame(code byte) (*Packet, error) {
	fc := d.frameCodes[code]
	flags := fc.flags
	if flags&flagInvalid != 0 {
		return nil, fmt.Errorf("%w: invalid frame code %d", ErrFormat, code)
	}
	if flags&flagCoded != 0 {
		v, err := readV(d.r)
		if err != nil {
			return nil, err
		}
		flags ^= v
	}
	streamID := fc.streamID
	if flags&flagStreamID != 0 {
		v, err := readV(d.r)
		if err != nil {
			return nil, err
		}
		streamID = int(v)
	}
	if streamID < 0 || streamID >= len(d.streams) || d.streams[streamID] == nil {
		return nil, fmt.Errorf("%w: frame for unknown stream %d", ErrFormat, streamID)
	}
	s := d.streams[streamID]
	var pts int64
	if flags&flagCodedPTS != 0 {
		coded, err := readV(d.r)
		if err != nil {
			return nil, err
		}
		if coded < 1<<s.msbPtsShift {
			mask := int64(1)<<s.msbPtsShift - 1
			delta := s.lastPts - mask/2
			pts = ((int64(coded) - delta) & mask) + delta
		} else {
			pts = int64(coded) - int64(1)<<s.msbPtsShift
		}
	} else {
		pts = s.lastPts + fc.ptsDelta
	}
	size := fc.sizeLSB
	if flags&flagSizeMSB != 0 {
		v, err := readV(d.r)
		if err != nil {
			return nil, err
		}
		size += fc.sizeMul * v
	}
	if flags&flagMatchTime != 0 {
		if _, err := readS(d.r); err != nil {
			return nil, err
		}
	}
	headerIdx := fc.headerIdx
	if flags&flagHeaderIdx != 0 {
		v, err := readV(d.r)
		if err != nil {
			return nil, err
		}
		headerIdx = int(v)
	}
	reserved := fc.reservedCount
	if flags&flagReserved != 0 {
		v, err := readV(d.r)
		if err != nil {
			return nil, err
		}
		reserved = v
	}
	for i := uint64(0); i < reserved; i++ {
		if _, err := readV(d.r); err != nil {
			return nil, err
		}
	}
	if headerIdx >= len(d.headers) {
		return nil, fmt.Errorf("%w: header_idx", ErrFormat)
	}
	if size > 4096 {
		headerIdx = 0
	}
	hdr := d.headers[headerIdx]
	if size < uint64(len(hdr)) {
		return nil, fmt.Errorf("%w: frame smaller than elided header", ErrFormat)
	}
	size -= uint64(len(hdr))
	if flags&flagChecksum != 0 {
		if _, err := io.ReadFull(d.r, make([]byte, 4)); err != nil {
			return nil, err
		}
	}
	if flags&flagSMData != 0 {
		return nil, fmt.Errorf("%w: side data frames (NUT v4) unsupported", ErrFormat)
	}
	if size > uint64(d.maxSize) {
		return nil, fmt.Errorf("%w: frame of %d bytes exceeds limit", ErrFormat, size)
	}
	data := make([]byte, len(hdr)+int(size))
	copy(data, hdr)
	if _, err := io.ReadFull(d.r, data[len(hdr):]); err != nil {
		return nil, err
	}
	s.lastPts = pts
	return &Packet{Stream: streamID, Pts: pts, Key: flags&flagKey != 0, Data: data}, nil
}

type byteReader interface{ ReadByte() (byte, error) }

func readV(r byteReader) (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v = v<<7 | uint64(b&0x7f)
		if b&0x80 == 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%w: varint overflow", ErrFormat)
}

func readS(r byteReader) (int64, error) {
	v, err := readV(r)
	if err != nil {
		return 0, err
	}
	v++
	if v&1 != 0 {
		return -int64(v >> 1), nil
	}
	return int64(v >> 1), nil
}
