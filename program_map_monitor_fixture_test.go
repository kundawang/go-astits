package astits

import (
	"bytes"
	"context"

	"github.com/asticode/go-astikit"
)

// tsEmitter builds a transport stream byte by byte. It keeps a continuity
// counter per PID so dynamic PAT/PMT fixtures can be assembled section by
// section.
type tsEmitter struct {
	buf bytes.Buffer
	cc  map[uint16]uint8
}

func newTSEmitter() *tsEmitter {
	return &tsEmitter{cc: make(map[uint16]uint8)}
}

// emitPSISection wraps a single PSI section payload in one TS packet.
func (e *tsEmitter) emitPSISection(pid uint16, payload []byte) {
	p := &Packet{
		Header: PacketHeader{
			ContinuityCounter:         e.cc[pid],
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       pid,
		},
		Payload: payload,
	}
	e.cc[pid]++
	if _, err := writePacket(astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &e.buf}), p, MpegTsPacketSize); err != nil {
		panic(err)
	}
}

// emitPAT emits one PAT section carrying the given programs.
func (e *tsEmitter) emitPAT(tsid uint16, version uint8, sectionNumber, lastSection uint8, programs []*PATProgram) {
	e.emitPSISection(PIDPAT, psiSectionBytes(PSITableIDPAT, tsid, version, sectionNumber, lastSection,
		&PSISectionSyntaxData{PAT: &PATData{Programs: programs, TransportStreamID: tsid}}))
}

// emitPMT emits one PMT section carrying the given elementary streams.
func (e *tsEmitter) emitPMT(programNumber, pmtPID uint16, version uint8, sectionNumber, lastSection uint8, pcrPID uint16, streams []*PMTElementaryStream) {
	e.emitPSISection(pmtPID, psiSectionBytes(PSITableIDPMT, programNumber, version, sectionNumber, lastSection,
		&PSISectionSyntaxData{PMT: &PMTData{
			ElementaryStreams:  streams,
			PCRPID:             pcrPID,
			ProgramNumber:      programNumber,
			ProgramDescriptors: []*Descriptor{},
		}}))
}

// emitBadCRCSection emits a section whose CRC32 field has been zeroed. The
// section parses normally up to the CRC check and fails exactly there, without
// leaving residual bytes that would disturb the next table. The PSI parser
// rejects it before the program map monitor can see it.
func (e *tsEmitter) emitBadCRCSection(pid uint16, payload []byte) {
	corrupted := make([]byte, len(payload))
	copy(corrupted, payload)
	// The CRC32 occupies the last 4 bytes of the section.
	if len(corrupted) < 4 {
		panic("emitBadCRCSection: section too short to corrupt")
	}
	for i := len(corrupted) - 4; i < len(corrupted); i++ {
		corrupted[i] = 0
	}
	e.emitPSISection(pid, corrupted)
}

// psiSectionBytes serializes one PAT/PMT section with a valid CRC32, prefixed
// by a zero pointer field.
func psiSectionBytes(tableID PSITableID, tableIDExtension uint16, version, sectionNumber, lastSection uint8, data *PSISectionSyntaxData) []byte {
	var sectionLength uint16
	switch tableID {
	case PSITableIDPAT:
		sectionLength = calcPATSectionLength(data.PAT)
	case PSITableIDPMT:
		sectionLength = calcPMTSectionLength(data.PMT)
	}
	// calcPSISectionLength adds the 5 syntax header bytes and the 4 CRC bytes on
	// top of the table-specific payload length.
	sectionLength += 5 + 4

	section := &PSISection{
		Header: &PSISectionHeader{
			SectionLength:          sectionLength,
			SectionSyntaxIndicator: true,
			TableID:                tableID,
		},
		Syntax: &PSISectionSyntax{
			Data: data,
			Header: &PSISectionSyntaxHeader{
				CurrentNextIndicator: true,
				LastSectionNumber:    lastSection,
				SectionNumber:        sectionNumber,
				TableIDExtension:     uint16(tableIDExtension),
				VersionNumber:        version,
			},
		},
	}

	buf := &bytes.Buffer{}
	if _, err := writePSIData(astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf}),
		&PSIData{Sections: []*PSISection{section}}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// patProgram is a fixture shorthand.
func patProgram(number, pmtPID uint16) *PATProgram {
	return &PATProgram{ProgramNumber: number, ProgramMapID: pmtPID}
}

// es is a fixture shorthand for an elementary stream without descriptors.
func es(streamType StreamType, pid uint16) *PMTElementaryStream {
	return &PMTElementaryStream{
		ElementaryPID:               pid,
		ElementaryStreamDescriptors: []*Descriptor{},
		StreamType:                  streamType,
	}
}

// runDemuxer drains every DemuxerData of the given stream and returns the
// program map events it produced. Parsing errors (e.g. a corrupted CRC) are
// collected separately instead of stopping the drain, mirroring a live
// receiver that keeps reading after a bad table.
func runDemuxer(ts []byte) (events []*ProgramMapEvent, parseErrors []error) {
	dmx := NewDemuxer(context.Background(), bytes.NewReader(ts), DemuxerOptPacketSize(MpegTsPacketSize))
	for {
		d, err := dmx.NextData()
		if err != nil {
			if err == ErrNoMorePackets {
				return
			}
			parseErrors = append(parseErrors, err)
			continue
		}
		if d.ProgramMapEvent != nil {
			events = append(events, d.ProgramMapEvent)
		}
	}
}
