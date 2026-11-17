package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adtsTestFrame builds a valid ADTS frame (AAC LC, 48 kHz, stereo, no CRC)
// with a payload of payloadLen bytes filled with fill
func adtsTestFrame(payloadLen int, fill byte) []byte {
	frameLen := adtsMinHeaderLength + payloadLen
	b := make([]byte, frameLen)
	b[0] = 0xff
	b[1] = 0xf1                   // syncword, MPEG-4, layer 0, protection absent
	b[2] = 0x40 | 3<<2            // profile AAC LC, sampling frequency index 3 (48000 Hz)
	b[3] = 0x80                   // channel configuration 2 (stereo)
	b[3] |= uint8(frameLen >> 11) // frame length, 13 bits
	b[4] = uint8(frameLen >> 3)
	b[5] = uint8(frameLen&0x7)<<5 | 0x1f
	b[6] = 0xfc // adts_buffer_fullness 0x7ff, 0 raw data blocks
	for i := adtsMinHeaderLength; i < frameLen; i++ {
		b[i] = fill
	}
	return b
}

// pesTestBytes builds a PES packet (header + payload) holding payload
func pesTestBytes(t *testing.T, pts *ClockReference, payload []byte) []byte {
	h := &PESHeader{
		StreamID: 0xc0,
		OptionalHeader: &PESOptionalHeader{
			MarkerBits: 2,
		},
	}
	if pts != nil {
		h.OptionalHeader.PTSDTSIndicator = PTSDTSIndicatorOnlyPTS
		h.OptionalHeader.PTS = pts
	}
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	_, _, err := writePESData(w, h, payload, true, 1024)
	require.NoError(t, err)
	return buf.Bytes()
}

// tsTestPacket builds a 188-byte TS packet carrying payload, zero-padded
func tsTestPacket(pid uint16, cc uint8, payload []byte) []byte {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	w.Write(uint8(syncByte))
	w.Write(packetHeaderBytes(PacketHeader{
		ContinuityCounter:         cc,
		HasPayload:                true,
		PayloadUnitStartIndicator: true,
		PID:                       pid,
	}, "01"))
	p := append(append([]byte{}, payload...), bytes.Repeat([]byte{0}, 184-len(payload))...)
	w.Write(p)
	return buf.Bytes()
}

// pesTestData builds a DemuxerData carrying a PES payload, without a TS layer
func pesTestData(pid uint16, pts *ClockReference, payload []byte, discontinuity bool) *DemuxerData {
	d := &DemuxerData{
		FirstPacket: &Packet{
			AdaptationField: &PacketAdaptationField{DiscontinuityIndicator: discontinuity},
		},
		PES: &PESData{
			Data: payload,
			Header: &PESHeader{
				StreamID:       0xc0,
				OptionalHeader: &PESOptionalHeader{MarkerBits: 2},
			},
		},
		PID: pid,
	}
	if pts != nil {
		d.PES.Header.OptionalHeader.PTSDTSIndicator = PTSDTSIndicatorOnlyPTS
		d.PES.Header.OptionalHeader.PTS = pts
	}
	return d
}

func ptsBase(f *ADTSFrame) int64 {
	if f.PTS == nil {
		return -1
	}
	return f.PTS.Base
}

// TestADTSExtractorTSFixture demuxes a fixed TS fixture holding three ADTS
// frames split across two PES (the PES boundary falls in the middle of frame
// 2) and checks the reassembled frame bytes, the per-frame PTS and that the
// regular demuxer parsing of the same fixture is left untouched
func TestADTSExtractorTSFixture(t *testing.T) {
	// PAT: program 1 -> PMT PID 0x1000
	pat := hexToBytes(`474000100000b00d0001c100000001f0002ab104b2ffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffff`)
	// PMT: H264 video on PID 0x100, ADTS AAC audio on PID 0x101
	pmt := hexToBytes(`475000100002b0170001c10000e100f0001be100f0000fe101f0002f44
		b99bffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		ffffffffffffffffff`)

	// Three frames of 1024 samples each at 48 kHz: 1920 ticks of the 90 kHz clock per frame
	f1 := adtsTestFrame(13, 0x11)
	f2 := adtsTestFrame(17, 0x22)
	f3 := adtsTestFrame(11, 0x33)

	// PES 1 carries a PTS and holds frame 1 + the first 10 bytes of frame 2
	pes1Payload := append(append([]byte{}, f1...), f2[:10]...)
	pes1 := pesTestBytes(t, newClockReference(9000, 0), pes1Payload)
	// PES 2 carries no PTS and holds the rest of frame 2 + frame 3
	pes2Payload := append(append([]byte{}, f2[10:]...), f3...)
	pes2 := pesTestBytes(t, nil, pes2Payload)

	// Build the fixture
	p1 := tsTestPacket(0x101, 0, pes1)
	p2 := tsTestPacket(0x101, 1, pes2)
	stream := append(append(append(append([]byte{}, pat...), pmt...), p1...), p2...)

	// Demux
	dmx := NewDemuxer(context.Background(), bytes.NewReader(stream), DemuxerOptPacketSize(188))
	var ds []*DemuxerData
	for {
		d, err := dmx.NextData()
		if err == ErrNoMorePackets {
			break
		}
		require.NoError(t, err)
		ds = append(ds, d)
	}
	require.Len(t, ds, 4)

	// The regular parsing is untouched: PAT, PMT and both raw PES payloads
	assert.NotNil(t, ds[0].PAT)
	require.NotNil(t, ds[1].PMT)
	require.Len(t, ds[1].PMT.ElementaryStreams, 2)
	assert.Equal(t, StreamTypeADTS, ds[1].PMT.ElementaryStreams[1].StreamType)
	assert.Equal(t, uint16(0x101), ds[1].PMT.ElementaryStreams[1].ElementaryPID)
	require.NotNil(t, ds[2].PES)
	assert.Equal(t, pes1Payload, ds[2].PES.Data)
	assert.Equal(t, int64(9000), ds[2].PES.Header.OptionalHeader.PTS.Base)
	require.NotNil(t, ds[3].PES)
	assert.Equal(t, pes2Payload, ds[3].PES.Data)
	assert.Nil(t, ds[3].PES.Header.OptionalHeader.PTS)

	// Extract ADTS frames
	e := NewADTSExtractor()
	var frames []*ADTSFrame
	for _, d := range ds {
		frames = append(frames, e.HandleData(d)...)
	}
	require.Len(t, frames, 3)

	// Frame bytes are the original ones, even for the frame split across PES
	assert.Equal(t, f1, frames[0].Data)
	assert.Equal(t, f2, frames[1].Data)
	assert.Equal(t, f3, frames[2].Data)

	// Config
	for _, f := range frames {
		assert.Equal(t, 1024, f.Samples)
		assert.Equal(t, 48000, f.Header.SampleRate)
		assert.Equal(t, uint8(2), f.Header.ChannelConfiguration)
		assert.Equal(t, uint8(1), f.Header.Profile)
	}

	// PTS: the PES PTS stamps frame 1, later frames advance by sample count
	assert.Equal(t, int64(9000), ptsBase(frames[0]))
	assert.Equal(t, int64(9000+1920), ptsBase(frames[1]))
	assert.Equal(t, int64(9000+2*1920), ptsBase(frames[2]))
}

func TestADTSExtractorPTSWrap(t *testing.T) {
	e := NewADTSExtractor()
	e.RegisterPID(0x101)

	// PTS one frame before the 33-bit wrap
	base := ptsWrapPeriod - 1920
	var payload []byte
	for i := 0; i < 3; i++ {
		payload = append(payload, adtsTestFrame(13, byte(i))...)
	}
	frames := e.HandleData(pesTestData(0x101, newClockReference(base, 0), payload, false))
	require.Len(t, frames, 3)

	// Timestamps are unwrapped and keep increasing past 2^33
	assert.Equal(t, base, ptsBase(frames[0]))
	assert.Equal(t, base+1920, ptsBase(frames[1]))
	assert.Equal(t, base+2*1920, ptsBase(frames[2]))
	assert.Greater(t, ptsBase(frames[2]), ptsWrapPeriod)

	// A new PES PTS on the other side of the wrap is unwrapped against the previous one
	frames = e.HandleData(pesTestData(0x101, newClockReference(1920, 0), adtsTestFrame(13, 0x42), false))
	require.Len(t, frames, 1)
	assert.Equal(t, ptsWrapPeriod+1920, ptsBase(frames[0]))
}

func TestADTSExtractorBadSyncword(t *testing.T) {
	e := NewADTSExtractor()
	e.RegisterPID(0x101)

	// Garbage before and between frames is dropped up to the next syncword
	f1 := adtsTestFrame(13, 0x11)
	f2 := adtsTestFrame(13, 0x22)
	payload := append([]byte{0x00, 0x01, 0x02}, f1...)
	payload = append(payload, 0x03, 0x04)
	payload = append(payload, f2...)
	frames := e.HandleData(pesTestData(0x101, newClockReference(9000, 0), payload, false))
	require.Len(t, frames, 2)
	assert.Equal(t, f1, frames[0].Data)
	assert.Equal(t, f2, frames[1].Data)
	assert.Equal(t, 5, e.DroppedBytes(0x101))
	assert.Equal(t, int64(9000), ptsBase(frames[0]))
	assert.Equal(t, int64(9000+1920), ptsBase(frames[1]))
}

func TestADTSExtractorInvalidFrameLength(t *testing.T) {
	e := NewADTSExtractor()
	e.RegisterPID(0x101)

	// A header whose frame length is smaller than the header itself
	bogus := []byte{0xff, 0xf1, 0x4c, 0x80, 0x00, 0x60, 0xfc} // frame length 4
	f := adtsTestFrame(13, 0x11)
	payload := append(bogus, f...)
	frames := e.HandleData(pesTestData(0x101, newClockReference(9000, 0), payload, false))
	require.Len(t, frames, 1)
	assert.Equal(t, f, frames[0].Data)
	assert.Equal(t, int64(9000), ptsBase(frames[0]))
	assert.Greater(t, e.DroppedBytes(0x101), 0)
}

func TestADTSExtractorTruncatedLastFrame(t *testing.T) {
	e := NewADTSExtractor()
	e.RegisterPID(0x101)

	// A truncated frame at the end of a PES is kept as residual, not emitted
	f := adtsTestFrame(17, 0x11)
	frames := e.HandleData(pesTestData(0x101, newClockReference(9000, 0), f[:10], false))
	assert.Empty(t, frames)

	// It is completed by the next PES and keeps the PES PTS of its first byte's PES
	frames = e.HandleData(pesTestData(0x101, nil, f[10:], false))
	require.Len(t, frames, 1)
	assert.Equal(t, f, frames[0].Data)
	assert.Equal(t, int64(9000), ptsBase(frames[0]))

	// A truncated frame at the end of the stream is simply never emitted
	frames = e.HandleData(pesTestData(0x101, nil, adtsTestFrame(17, 0x22)[:5], false))
	assert.Empty(t, frames)
}

func TestADTSExtractorDiscontinuity(t *testing.T) {
	e := NewADTSExtractor()
	e.RegisterPID(0x101)

	// Partial frame followed by a discontinuity: the residual is dropped
	f1 := adtsTestFrame(17, 0x11)
	frames := e.HandleData(pesTestData(0x101, newClockReference(9000, 0), f1[:10], false))
	assert.Empty(t, frames)

	f2 := adtsTestFrame(13, 0x22)
	frames = e.HandleData(pesTestData(0x101, newClockReference(36000, 0), f2, true))
	require.Len(t, frames, 1)
	assert.Equal(t, f2, frames[0].Data)
	// The timeline restarts from the PTS of the discontinuous PES
	assert.Equal(t, int64(36000), ptsBase(frames[0]))
	assert.Equal(t, 10, e.DroppedBytes(0x101))
}

func TestADTSExtractorNoPTSStart(t *testing.T) {
	e := NewADTSExtractor()
	e.RegisterPID(0x101)

	// Frames before the first PTS have no timestamp
	f1 := adtsTestFrame(13, 0x11)
	frames := e.HandleData(pesTestData(0x101, nil, f1, false))
	require.Len(t, frames, 1)
	assert.Nil(t, frames[0].PTS)

	// A PES PTS stamps the first frame starting in that PES payload, not the
	// frame completed from the residual of the previous one
	f2 := adtsTestFrame(13, 0x22)
	f3 := adtsTestFrame(13, 0x33)
	frames = e.HandleData(pesTestData(0x101, nil, f2[:8], false))
	assert.Empty(t, frames)
	frames = e.HandleData(pesTestData(0x101, newClockReference(9000, 0), append(f2[8:], f3...), false))
	require.Len(t, frames, 2)
	assert.Nil(t, frames[0].PTS) // f2 started in the previous PES
	assert.Equal(t, int64(9000), ptsBase(frames[1]))
}

func TestADTSExtractorPIDSwitch(t *testing.T) {
	e := NewADTSExtractor()
	e.RegisterPID(0x101)
	e.RegisterPID(0x102)

	// Interleaved PES of two PIDs keep independent residuals and timelines
	a1 := adtsTestFrame(13, 0xa1)
	a2 := adtsTestFrame(13, 0xa2)
	b1 := adtsTestFrame(17, 0xb1)

	frames := e.HandleData(pesTestData(0x101, newClockReference(9000, 0), a1[:8], false))
	assert.Empty(t, frames)
	frames = e.HandleData(pesTestData(0x102, newClockReference(45000, 0), b1, false))
	require.Len(t, frames, 1)
	assert.Equal(t, b1, frames[0].Data)
	assert.Equal(t, int64(45000), ptsBase(frames[0]))

	// PID 0x101 resumes where it left off, unaffected by PID 0x102
	payload := append(a1[8:], a2...)
	frames = e.HandleData(pesTestData(0x101, nil, payload, false))
	require.Len(t, frames, 2)
	assert.Equal(t, a1, frames[0].Data)
	assert.Equal(t, a2, frames[1].Data)
	assert.Equal(t, int64(9000), ptsBase(frames[0]))
	assert.Equal(t, int64(9000+1920), ptsBase(frames[1]))
}

func TestParseADTSHeader(t *testing.T) {
	// Valid header
	h, err := parseADTSHeader(adtsTestFrame(13, 0x00))
	assert.NoError(t, err)
	assert.Equal(t, 20, h.FrameLength)
	assert.Equal(t, 48000, h.SampleRate)
	assert.Equal(t, uint8(3), h.SamplingFrequencyIndex)
	assert.Equal(t, uint8(2), h.ChannelConfiguration)
	assert.Equal(t, uint8(1), h.Profile)
	assert.False(t, h.HasCRC)
	assert.Equal(t, 1024, h.SamplesPerFrame())

	// Bad syncword
	_, err = parseADTSHeader([]byte{0x00, 0xf1, 0x4c, 0x80, 0x00, 0x60, 0xfc})
	assert.True(t, errors.Is(err, ErrADTSInvalidSyncword))

	// Bad layer
	_, err = parseADTSHeader([]byte{0xff, 0xf3, 0x4c, 0x80, 0x00, 0x60, 0xfc})
	assert.True(t, errors.Is(err, ErrADTSInvalidLayer))

	// Reserved sampling frequency index
	_, err = parseADTSHeader([]byte{0xff, 0xf1, 0x7c, 0x80, 0x00, 0x60, 0xfc})
	assert.True(t, errors.Is(err, ErrADTSInvalidSampleRate))

	// Frame length smaller than the header
	_, err = parseADTSHeader([]byte{0xff, 0xf1, 0x4c, 0x80, 0x00, 0x60, 0xfc})
	assert.True(t, errors.Is(err, ErrADTSInvalidFrameLen))
}
