package astits

import (
	"errors"
	"fmt"
)

// ADTS constants
// https://wiki.multimedia.cx/index.php/ADTS
// ISO/IEC 13818-7
const (
	adtsMinHeaderLength = 7 // without CRC
	adtsMaxHeaderLength = 9 // with CRC
	adtsSyncword        = 0xfff
	adtsMaxFrameLength  = 1 << 13 // frame_length is 13 bits, header included
	adtsSamplesPerBlock = 1024
)

// ptsWrapPeriod is the wrap period of the 33-bit PTS in 90 kHz units
const ptsWrapPeriod = int64(1) << 33

// adtsSampleRates maps sampling_frequency_index to a sample rate in Hz
var adtsSampleRates = []int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350,
}

// Errors
var (
	ErrADTSInvalidSyncword   = errors.New("astits: invalid ADTS syncword")
	ErrADTSInvalidLayer      = errors.New("astits: invalid ADTS layer")
	ErrADTSInvalidSampleRate = errors.New("astits: invalid ADTS sampling frequency index")
	ErrADTSInvalidFrameLen   = errors.New("astits: invalid ADTS frame length")
)

// ADTSHeader represents the fixed header of an ADTS frame
type ADTSHeader struct {
	ChannelConfiguration         uint8
	FrameLength                  int // Total frame length in bytes, header included
	HasCRC                       bool
	NumberOfRawDataBlocksInFrame uint8
	Profile                      uint8 // 0: Main, 1: LC, 2: SSR, 3: reserved
	SampleRate                   int   // In Hz
	SamplingFrequencyIndex       uint8
}

// SamplesPerFrame returns the number of samples per channel carried by the frame
func (h ADTSHeader) SamplesPerFrame() int {
	return adtsSamplesPerBlock * (int(h.NumberOfRawDataBlocksInFrame) + 1)
}

// ADTSFrame represents a complete ADTS frame reassembled from PES payloads
type ADTSFrame struct {
	Data    []byte          // Raw frame, ADTS header included
	Header  ADTSHeader      // Parsed fixed header
	PTS     *ClockReference // nil when no PTS is known yet for this PID
	Samples int             // Number of samples per channel
}

// parseADTSHeader parses an ADTS fixed header from the beginning of b.
// b must hold at least adtsMinHeaderLength bytes.
func parseADTSHeader(b []byte) (h ADTSHeader, err error) {
	// Syncword: 12 bits, all ones
	if len(b) < adtsMinHeaderLength || b[0] != 0xff || b[1]&0xf0 != 0xf0 {
		err = ErrADTSInvalidSyncword
		return
	}

	// Layer: must be 0
	if b[1]&0x6 != 0 {
		err = ErrADTSInvalidLayer
		return
	}

	h.HasCRC = b[1]&0x1 == 0 // protection_absent == 0 means a 16-bit CRC follows the fixed header
	h.Profile = b[2] >> 6
	h.SamplingFrequencyIndex = b[2] >> 2 & 0xf
	if int(h.SamplingFrequencyIndex) >= len(adtsSampleRates) {
		err = ErrADTSInvalidSampleRate
		return
	}
	h.SampleRate = adtsSampleRates[h.SamplingFrequencyIndex]
	h.ChannelConfiguration = (b[2]&0x1)<<2 | b[3]>>6
	h.FrameLength = int(b[3]&0x3)<<11 | int(b[4])<<3 | int(b[5])>>5
	h.NumberOfRawDataBlocksInFrame = b[6] & 0x3

	// Frame length must at least cover the header
	headerLength := adtsMinHeaderLength
	if h.HasCRC {
		headerLength = adtsMaxHeaderLength
	}
	if h.FrameLength < headerLength || h.FrameLength >= adtsMaxFrameLength {
		err = fmt.Errorf("%w: %d", ErrADTSInvalidFrameLen, h.FrameLength)
		return
	}
	return
}

// adtsResyncOffset returns the offset of the next syncword candidate in b,
// starting the search at offset 1. If none is found it returns the number of
// bytes that can safely be dropped, keeping a trailing 0xff that may be the
// first byte of a syncword split across PES payloads.
func adtsResyncOffset(b []byte) int {
	for i := 1; i+1 < len(b); i++ {
		if b[i] == 0xff && b[i+1]&0xf0 == 0xf0 {
			return i
		}
	}
	if len(b) > 0 && b[len(b)-1] == 0xff {
		return len(b) - 1
	}
	return len(b)
}

// unwrapPTS returns the value congruent to pts modulo 2^33 that is closest to
// ref, so that a 33-bit PTS wrap is resolved into a monotonous 64-bit timeline
func unwrapPTS(pts, ref int64) int64 {
	d := (pts - ref) & (ptsWrapPeriod - 1)
	if d >= ptsWrapPeriod/2 {
		d -= ptsWrapPeriod
	}
	return ref + d
}

// adtsTimeline converts a flow of frames into monotonous 90 kHz timestamps.
// Time is anchored to a PTS and advanced by the sample count of each frame,
// which keeps rounding error from accumulating.
type adtsTimeline struct {
	anchor  int64 // 90 kHz timestamp of the anchor
	rate    int   // sample rate at the anchor
	samples int64 // samples elapsed since the anchor
	set     bool
}

// setAt re-anchors the timeline
func (t *adtsTimeline) setAt(pts int64, rate int) {
	t.anchor = pts
	t.rate = rate
	t.samples = 0
	t.set = true
}

// pts returns the current 90 kHz timestamp
func (t *adtsTimeline) pts() int64 {
	return t.anchor + t.samples*90000/int64(t.rate)
}

// advance moves the timeline forward by one frame
func (t *adtsTimeline) advance(h ADTSHeader) {
	if t.rate != h.SampleRate {
		// Sample rate changed: materialize the current position and re-anchor
		t.setAt(t.pts(), h.SampleRate)
	}
	t.samples += int64(h.SamplesPerFrame())
}

// adtsPIDState holds the reassembly state of a single PID
type adtsPIDState struct {
	buf           []byte // residual bytes of an incomplete frame, kept across PES payloads
	droppedBytes  int    // bytes dropped while resynchronizing
	hasPendingPTS bool
	hasPESPTS     bool
	lastPESPTS    int64 // unwrapped PTS of the last PES that carried one
	pendingOffset int   // bytes of buf that precede the payload the pending PTS belongs to
	pendingPTS    int64 // unwrapped PTS to stamp on the first frame starting at/after pendingOffset
	timeline      adtsTimeline
}

// ADTSExtractor reassembles ADTS frames from the PES payloads of PIDs that a
// PMT flagged as ADTS (StreamTypeADTS). PES boundaries may fall in the middle
// of a frame: residual bytes are kept per PID and completed by the next PES.
//
// Error handling rules:
//   - bad syncword: bytes are dropped one by one until the next syncword
//     candidate; a trailing 0xff is kept since it may start a syncword split
//     across PES payloads
//   - abnormal frame length (smaller than the header): the sync byte is
//     dropped and resynchronization restarts at the next byte
//   - truncated last frame: bytes are kept as residual and completed by the
//     next PES; they are dropped at the end of the stream or on discontinuity
//   - PES discontinuity (adaptation field discontinuity indicator): residual
//     bytes and pending timing are dropped; the timeline restarts from the
//     next PES PTS if any
//   - no PTS yet: frames are emitted with a nil PTS until a PES carries one;
//     that PTS stamps the first frame whose first byte lies in this PES
//     payload, later frames are advanced by their sample count
//
// State is kept per PID, so interleaved PES of several ADTS PIDs (PID
// switching) are handled independently.
type ADTSExtractor struct {
	pids map[uint16]*adtsPIDState
}

// NewADTSExtractor creates a new ADTS extractor
func NewADTSExtractor() *ADTSExtractor {
	return &ADTSExtractor{pids: make(map[uint16]*adtsPIDState)}
}

// RegisterPID registers a PID as carrying ADTS audio. Useful when no PMT is
// available; PIDs flagged as ADTS in a PMT are registered automatically by
// HandleData.
func (e *ADTSExtractor) RegisterPID(pid uint16) {
	e.state(pid)
}

// DroppedBytes returns the number of bytes dropped while resynchronizing a PID
func (e *ADTSExtractor) DroppedBytes(pid uint16) int {
	if s, ok := e.pids[pid]; ok {
		return s.droppedBytes
	}
	return 0
}

// HandleData processes a DemuxerData and returns the ADTS frames completed by
// it, if any. PMT data registers ADTS PIDs, PES data of registered PIDs is
// reassembled into frames, anything else is ignored.
func (e *ADTSExtractor) HandleData(d *DemuxerData) (frames []*ADTSFrame) {
	if d == nil {
		return
	}
	if d.PMT != nil {
		for _, es := range d.PMT.ElementaryStreams {
			if es.StreamType == StreamTypeADTS {
				e.state(es.ElementaryPID)
			}
		}
		return
	}
	if d.PES == nil {
		return
	}
	s, ok := e.pids[d.PID]
	if !ok {
		return
	}
	discontinuity := d.FirstPacket != nil &&
		d.FirstPacket.AdaptationField != nil &&
		d.FirstPacket.AdaptationField.DiscontinuityIndicator
	return s.pushPES(d.PES, discontinuity)
}

func (e *ADTSExtractor) state(pid uint16) *adtsPIDState {
	s, ok := e.pids[pid]
	if !ok {
		s = &adtsPIDState{}
		e.pids[pid] = s
	}
	return s
}

// pushPES feeds one PES payload into the PID state
func (s *adtsPIDState) pushPES(d *PESData, discontinuity bool) []*ADTSFrame {
	// Discontinuity: drop residual bytes and pending timing, the timeline
	// restarts from the PTS of this PES if it carries one
	if discontinuity {
		s.droppedBytes += len(s.buf)
		s.buf = s.buf[:0]
		s.hasPendingPTS = false
		s.timeline.set = false
	}

	// A PES PTS stamps the first frame whose first byte lies in this payload,
	// i.e. the first frame starting at or after the current residual length
	if d.Header != nil && d.Header.OptionalHeader != nil && d.Header.OptionalHeader.PTS != nil {
		pts := d.Header.OptionalHeader.PTS.Base
		if s.hasPESPTS {
			pts = unwrapPTS(pts, s.lastPESPTS)
		}
		s.lastPESPTS = pts
		s.hasPESPTS = true
		s.pendingPTS = pts
		s.pendingOffset = len(s.buf)
		s.hasPendingPTS = true
	}

	s.buf = append(s.buf, d.Data...)
	return s.drain()
}

// drain extracts all complete frames from the residual buffer
func (s *adtsPIDState) drain() (frames []*ADTSFrame) {
	b := s.buf
	var off int
	consume := func(n int) {
		off += n
		if s.hasPendingPTS {
			s.pendingOffset -= n
		}
	}
	for {
		// Not enough bytes to read a header: wait for more
		if len(b)-off < adtsMinHeaderLength {
			break
		}

		// Bad syncword: drop bytes up to the next syncword candidate
		if b[off] != 0xff || b[off+1]&0xf0 != 0xf0 {
			n := adtsResyncOffset(b[off:])
			s.droppedBytes += n
			consume(n)
			continue
		}

		// Invalid header (abnormal frame length, bad layer or sample rate):
		// drop the sync byte and resynchronize from the next byte
		h, err := parseADTSHeader(b[off:])
		if err != nil {
			s.droppedBytes++
			consume(1)
			continue
		}

		// Truncated frame: keep the residual and wait for the next PES
		if len(b)-off < h.FrameLength {
			break
		}

		// Build frame
		data := make([]byte, h.FrameLength)
		copy(data, b[off:])
		f := &ADTSFrame{
			Data:    data,
			Header:  h,
			Samples: h.SamplesPerFrame(),
		}

		// A pending PES PTS applies to the first frame starting at or after
		// the payload boundary it was carried with
		if s.hasPendingPTS && s.pendingOffset <= 0 {
			s.timeline.setAt(s.pendingPTS, h.SampleRate)
			s.hasPendingPTS = false
		}
		if s.timeline.set {
			f.PTS = newClockReference(s.timeline.pts(), 0)
			s.timeline.advance(h)
		}

		frames = append(frames, f)
		consume(h.FrameLength)
	}

	// Compact residual
	s.buf = append(s.buf[:0], b[off:]...)
	return
}
