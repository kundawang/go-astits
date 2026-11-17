package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// demuxerDataSection wraps a section into the DemuxerData shape the monitor
// consumes.
func monitorData(pid uint16, s *PSISection) *DemuxerData {
	return &DemuxerData{FirstPacket: &Packet{Header: PacketHeader{PID: pid}}, PID: pid, Section: s}
}

func patSectionData(version, sectionNumber, lastSection uint8, tsid uint16, programs ...*PATProgram) *DemuxerData {
	return monitorData(PIDPAT, &PSISection{
		Header: &PSISectionHeader{TableID: PSITableIDPAT},
		Syntax: &PSISectionSyntax{
			Data: &PSISectionSyntaxData{PAT: &PATData{Programs: programs, TransportStreamID: tsid}},
			Header: &PSISectionSyntaxHeader{
				CurrentNextIndicator: true,
				LastSectionNumber:    lastSection,
				SectionNumber:        sectionNumber,
				TableIDExtension:     tsid,
				VersionNumber:        version,
			},
		},
	})
}

func pmtSectionData(version, sectionNumber, lastSection uint8, programNumber, pid uint16, streams ...*PMTElementaryStream) *DemuxerData {
	return monitorData(pid, &PSISection{
		Header: &PSISectionHeader{TableID: PSITableIDPMT},
		Syntax: &PSISectionSyntax{
			Data: &PSISectionSyntaxData{PMT: &PMTData{ElementaryStreams: streams, ProgramNumber: programNumber}},
			Header: &PSISectionSyntaxHeader{
				CurrentNextIndicator: true,
				LastSectionNumber:    lastSection,
				SectionNumber:        sectionNumber,
				TableIDExtension:     programNumber,
				VersionNumber:        version,
			},
		},
	})
}

// The first complete table only establishes the baseline and must not emit.
func TestProgramMapMonitorBaselineEmitsNothing(t *testing.T) {
	m := newProgramMapMonitor()

	events, err := m.handleSection(patSectionData(3, 0, 0, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	assert.Empty(t, events)

	events, err = m.handleSection(pmtSectionData(7, 0, 0, 1, 0x1000, es(StreamTypeH264Video, 0x100), es(StreamTypeADTS, 0x101)))
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Equal(t, uint8(3), m.patVersion)
	require.Contains(t, m.programs, uint32(1))
	assert.Equal(t, uint8(7), m.programs[1].pmtVersion)
}

// Multi-section PAT: nothing is committed until every section arrived.
func TestProgramMapMonitorMultiSectionPATWaitsThenCommits(t *testing.T) {
	m := newProgramMapMonitor()

	events, err := m.handleSection(patSectionData(2, 0, 1, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.NotNil(t, m.patAssembly)

	// Retransmitting section 0 of the same version changes nothing.
	events, err = m.handleSection(patSectionData(2, 0, 1, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	assert.Empty(t, events)

	// Completing the table establishes the baseline: no event.
	events, err = m.handleSection(patSectionData(2, 1, 1, 1, patProgram(2, 0x1001)))
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Nil(t, m.patAssembly)
	assert.Len(t, m.programs, 2)

	// Version 3 changes the PMT PID of program 1.
	events, err = m.handleSection(patSectionData(3, 0, 1, 1, patProgram(1, 0x2000)))
	require.NoError(t, err)
	assert.Empty(t, events)
	events, err = m.handleSection(patSectionData(3, 1, 1, 1, patProgram(2, 0x1001)))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, PSITableTypePAT, events[0].TableType)
	assert.Equal(t, uint8(2), events[0].PreviousVersion)
	assert.Equal(t, uint8(3), events[0].Version)
	assert.Equal(t, uint16(1), events[0].ProgramNumber)
	require.Len(t, events[0].Changes, 2)
	assert.Equal(t, ProgramMapChangeKindRemoved, events[0].Changes[0].Kind)
	assert.Equal(t, uint16(0x1000), events[0].Changes[0].PID)
	assert.Equal(t, ProgramMapChangeKindAdded, events[0].Changes[1].Kind)
	assert.Equal(t, uint16(0x2000), events[0].Changes[1].PID)
}

// A version conflict while reassembling discards the partial table and restarts
// with the conflicting section.
func TestProgramMapMonitorVersionConflictRestartsAssembly(t *testing.T) {
	m := newProgramMapMonitor()

	_, err := m.handleSection(patSectionData(1, 0, 1, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)

	// Section 1 arrives with a newer version: the partial assembly is discarded
	// and reassembly restarts with the conflicting section.
	events, err := m.handleSection(patSectionData(2, 1, 1, 1, patProgram(2, 0x1001)))
	require.NoError(t, err)
	assert.Empty(t, events)
	require.NotNil(t, m.patAssembly)
	assert.Equal(t, uint8(2), m.patAssembly.version)
	assert.Len(t, m.patAssembly.sections, 1)
	assert.Contains(t, m.patAssembly.sections, uint8(1))

	// Nothing committed yet.
	assert.Equal(t, psiNoVersion, m.patVersion)

	// Complete version 2; it becomes the baseline without an event.
	events, err = m.handleSection(patSectionData(2, 0, 1, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Equal(t, uint8(2), m.patVersion)
}

// A section number beyond last_section_number is rejected.
func TestProgramMapMonitorSectionOutOfRange(t *testing.T) {
	m := newProgramMapMonitor()

	_, err := m.handleSection(patSectionData(1, 2, 1, 1, patProgram(1, 0x1000)))
	require.Error(t, err)
	assert.Nil(t, m.patAssembly)
}

// last_section_number changing within one version is rejected.
func TestProgramMapMonitorLastSectionConflict(t *testing.T) {
	m := newProgramMapMonitor()

	_, err := m.handleSection(patSectionData(1, 0, 1, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	_, err = m.handleSection(patSectionData(1, 1, 2, 1, patProgram(2, 0x1001)))
	require.Error(t, err)
	assert.Len(t, m.patAssembly.sections, 1)
}

// Retransmitting an already committed version never reports again, including
// when the version number wraps from 31 to 0.
func TestProgramMapMonitorDuplicateAndWrappingVersion(t *testing.T) {
	m := newProgramMapMonitor()
	mustHandle := func(d *DemuxerData) []*ProgramMapEvent {
		events, err := m.handleSection(d)
		require.NoError(t, err)
		return events
	}

	// Baseline PAT version 31 with program 1.
	assert.Empty(t, mustHandle(patSectionData(31, 0, 0, 1, patProgram(1, 0x1000))))
	// Baseline PMT version 31: video + audio.
	assert.Empty(t, mustHandle(pmtSectionData(31, 0, 0, 1, 0x1000, es(StreamTypeH264Video, 0x100), es(StreamTypeADTS, 0x101))))

	// Exact retransmission of both tables: no events.
	assert.Empty(t, mustHandle(patSectionData(31, 0, 0, 1, patProgram(1, 0x1000))))
	assert.Empty(t, mustHandle(pmtSectionData(31, 0, 0, 1, 0x1000, es(StreamTypeH264Video, 0x100), es(StreamTypeADTS, 0x101))))

	// PMT version wraps to 0 and swaps the audio PID (the director switches
	// the audio track): one event with an added and a removed audio PID.
	events := mustHandle(pmtSectionData(0, 0, 0, 1, 0x1000, es(StreamTypeH264Video, 0x100), es(StreamTypeADTS, 0x102)))
	require.Len(t, events, 1)
	assert.Equal(t, uint8(31), events[0].PreviousVersion)
	assert.Equal(t, uint8(0), events[0].Version)
	require.Len(t, events[0].Changes, 2)
	assert.Equal(t, ProgramMapChangeKindAdded, events[0].Changes[0].Kind)
	assert.Equal(t, uint16(0x102), events[0].Changes[0].PID)
	assert.Equal(t, StreamTypeADTS, events[0].Changes[0].StreamType)
	assert.Equal(t, ProgramMapChangeKindRemoved, events[0].Changes[1].Kind)
	assert.Equal(t, uint16(0x101), events[0].Changes[1].PID)
	assert.Equal(t, StreamTypeADTS, events[0].Changes[1].PreviousStreamType)

	// Retransmission of wrapped version 0: no duplicate event.
	assert.Empty(t, mustHandle(pmtSectionData(0, 0, 0, 1, 0x1000, es(StreamTypeH264Video, 0x100), es(StreamTypeADTS, 0x102))))
}

// A PID reused with a different stream type is reported as type_changed.
func TestProgramMapMonitorPIDReuseTypeChanged(t *testing.T) {
	m := newProgramMapMonitor()

	_, err := m.handleSection(patSectionData(1, 0, 0, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	_, err = m.handleSection(pmtSectionData(1, 0, 0, 1, 0x1000, es(StreamTypeADTS, 0x101)))
	require.NoError(t, err)

	events, err := m.handleSection(pmtSectionData(2, 0, 0, 1, 0x1000, es(StreamTypeAC3Audio, 0x101)))
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Len(t, events[0].Changes, 1)
	c := events[0].Changes[0]
	assert.Equal(t, ProgramMapChangeKindTypeChanged, c.Kind)
	assert.Equal(t, uint16(0x101), c.PID)
	assert.Equal(t, StreamTypeADTS, c.PreviousStreamType)
	assert.Equal(t, StreamTypeAC3Audio, c.StreamType)
}

// PAT deleting a program reports its PMT PID and all its elementary PIDs, and
// frees every piece of state attached to the program.
func TestProgramMapMonitorPATDeletesProgram(t *testing.T) {
	m := newProgramMapMonitor()

	_, err := m.handleSection(patSectionData(1, 0, 0, 1, patProgram(1, 0x1000), patProgram(2, 0x1001)))
	require.NoError(t, err)
	_, err = m.handleSection(pmtSectionData(1, 0, 0, 1, 0x1000, es(StreamTypeH264Video, 0x100), es(StreamTypeADTS, 0x101)))
	require.NoError(t, err)
	_, err = m.handleSection(pmtSectionData(1, 0, 0, 2, 0x1001, es(StreamTypeH265Video, 0x200)))
	require.NoError(t, err)

	// Program 2 disappears from the PAT.
	events, err := m.handleSection(patSectionData(2, 0, 0, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, uint16(2), events[0].ProgramNumber)
	require.Len(t, events[0].Changes, 2)
	assert.Equal(t, ProgramMapChangeKindRemoved, events[0].Changes[0].Kind)
	assert.Equal(t, uint16(0x1001), events[0].Changes[0].PID)
	assert.Equal(t, ProgramMapChangeKindRemoved, events[0].Changes[1].Kind)
	assert.Equal(t, uint16(0x200), events[0].Changes[1].PID)
	assert.Equal(t, StreamTypeH265Video, events[0].Changes[1].PreviousStreamType)

	// State of the deleted program is gone.
	assert.NotContains(t, m.programs, uint32(2))
	assert.NotContains(t, m.pmtAssemblies, uint32(2))
	// Program 1 survives.
	assert.Contains(t, m.programs, uint32(1))

	// A PMT for the deleted program is now ignored instead of recreating state.
	events, err = m.handleSection(pmtSectionData(2, 0, 0, 2, 0x1001, es(StreamTypeH265Video, 0x200)))
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.NotContains(t, m.programs, uint32(2))
}

// Multi-section PMT: sections are merged and only the complete table commits.
func TestProgramMapMonitorMultiSectionPMT(t *testing.T) {
	m := newProgramMapMonitor()

	_, err := m.handleSection(patSectionData(1, 0, 0, 1, patProgram(1, 0x1000)))
	require.NoError(t, err)
	// Baseline PMT, two sections.
	_, err = m.handleSection(pmtSectionData(1, 0, 1, 1, 0x1000, es(StreamTypeH264Video, 0x100)))
	require.NoError(t, err)
	_, err = m.handleSection(pmtSectionData(1, 1, 1, 1, 0x1000, es(StreamTypeADTS, 0x101)))
	require.NoError(t, err)

	// New version changes the stream carried by section 1.
	_, err = m.handleSection(pmtSectionData(2, 0, 1, 1, 0x1000, es(StreamTypeH264Video, 0x100)))
	require.NoError(t, err)
	// No event while section 1 is missing.
	assert.Empty(t, m.pmtAssemblies[1].sections[1])
	events, err := m.handleSection(pmtSectionData(2, 1, 1, 1, 0x1000, es(StreamTypeEAC3Audio, 0x103)))
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Len(t, events[0].Changes, 2)
	assert.Equal(t, ProgramMapChangeKindAdded, events[0].Changes[0].Kind)
	assert.Equal(t, uint16(0x103), events[0].Changes[0].PID)
	assert.Equal(t, ProgramMapChangeKindRemoved, events[0].Changes[1].Kind)
	assert.Equal(t, uint16(0x101), events[0].Changes[1].PID)
}

// Sections with current_next_indicator == 0 describe a future table and must be
// ignored.
func TestProgramMapMonitorIgnoresFutureTable(t *testing.T) {
	m := newProgramMapMonitor()

	future := patSectionData(1, 0, 0, 1, patProgram(1, 0x1000))
	future.Section.Syntax.Header.CurrentNextIndicator = false
	events, err := m.handleSection(future)
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Nil(t, m.patAssembly)
	assert.Equal(t, psiNoVersion, m.patVersion)
}

// End to end through the Demuxer: audio track switch produces exactly one PMT
// event carrying the trigger position and both versions.
func TestDemuxerProgramMapEventAudioSwitch(t *testing.T) {
	e := newTSEmitter()
	e.emitPAT(1, 5, 0, 0, []*PATProgram{patProgram(1, 0x1000)})
	e.emitPMT(1, 0x1000, 9, 0, 0, 0x100, []*PMTElementaryStream{
		es(StreamTypeH264Video, 0x100),
		es(StreamTypeADTS, 0x101),
	})
	// Retransmission of the same versions.
	e.emitPAT(1, 5, 0, 0, []*PATProgram{patProgram(1, 0x1000)})
	e.emitPMT(1, 0x1000, 9, 0, 0, 0x100, []*PMTElementaryStream{
		es(StreamTypeH264Video, 0x100),
		es(StreamTypeADTS, 0x101),
	})
	// Director switches the audio track: PMT version 10, new audio PID.
	e.emitPMT(1, 0x1000, 10, 0, 0, 0x100, []*PMTElementaryStream{
		es(StreamTypeH264Video, 0x100),
		es(StreamTypeADTS, 0x102),
	})

	events, parseErrors := runDemuxer(e.buf.Bytes())
	require.Empty(t, parseErrors)
	require.Len(t, events, 1)

	ev := events[0]
	assert.Equal(t, PSITableTypePMT, ev.TableType)
	assert.Equal(t, uint16(1), ev.ProgramNumber)
	assert.Equal(t, uint16(1), ev.TableIDExtension)
	assert.Equal(t, uint8(9), ev.PreviousVersion)
	assert.Equal(t, uint8(10), ev.Version)
	assert.Equal(t, uint16(0x1000), ev.PID)
	require.NotNil(t, ev.FirstPacket)
	assert.Equal(t, uint16(0x1000), ev.FirstPacket.Header.PID)
	require.Len(t, ev.Changes, 2)
	assert.Equal(t, &ProgramMapChange{Kind: ProgramMapChangeKindAdded, PID: 0x102, StreamType: StreamTypeADTS}, ev.Changes[0])
	assert.Equal(t, &ProgramMapChange{Kind: ProgramMapChangeKindRemoved, PID: 0x101, PreviousStreamType: StreamTypeADTS}, ev.Changes[1])
}

// End to end: multi-section PAT, bad CRC and version wrapping.
func TestDemuxerProgramMapEventMultiSectionAndBadCRC(t *testing.T) {
	e := newTSEmitter()

	// Two-section PAT version 31.
	e.emitPAT(1, 31, 0, 1, []*PATProgram{patProgram(1, 0x1000)})
	// A corrupted copy of section 1 is rejected by the CRC check.
	bad := psiSectionBytes(PSITableIDPAT, 1, 31, 1, 1, &PSISectionSyntaxData{
		PAT: &PATData{Programs: []*PATProgram{patProgram(2, 0x1001)}, TransportStreamID: 1},
	})
	e.emitBadCRCSection(PIDPAT, bad)
	// Valid section 1 completes the baseline.
	e.emitPAT(1, 31, 1, 1, []*PATProgram{patProgram(2, 0x1001)})
	e.emitPMT(1, 0x1000, 31, 0, 0, 0x100, []*PMTElementaryStream{es(StreamTypeADTS, 0x101)})

	// PAT wraps to version 0 and removes program 2.
	e.emitPAT(1, 0, 0, 1, []*PATProgram{patProgram(1, 0x1000)})
	e.emitPAT(1, 0, 1, 1, []*PATProgram{})

	events, parseErrors := runDemuxer(e.buf.Bytes())
	// The corrupted section surfaced parse errors (CRC mismatch, plus the
	// truncated remainder of the packet) but did not stop the drain.
	require.NotEmpty(t, parseErrors)
	hasCRCError := false
	for _, perr := range parseErrors {
		if errors.Is(perr, ErrCRC32Mismatch) {
			hasCRCError = true
		}
	}
	assert.True(t, hasCRCError, "expected a CRC32 mismatch error, got %v", parseErrors)
	// The only mapping event is the wrapped PAT deleting program 2. The corrupt
	// table was never committed, so no spurious event was produced.
	require.Len(t, events, 1)
	assert.Equal(t, PSITableTypePAT, events[0].TableType)
	assert.Equal(t, uint8(31), events[0].PreviousVersion)
	assert.Equal(t, uint8(0), events[0].Version)
	assert.Equal(t, uint16(2), events[0].ProgramNumber)
	require.Len(t, events[0].Changes, 1)
	assert.Equal(t, ProgramMapChangeKindRemoved, events[0].Changes[0].Kind)
	assert.Equal(t, uint16(0x1001), events[0].Changes[0].PID)
}

// End to end: a missing section keeps the old version active until a complete
// retransmission arrives.
func TestDemuxerProgramMapEventWaitsForMissingSection(t *testing.T) {
	e := newTSEmitter()
	e.emitPAT(1, 1, 0, 0, []*PATProgram{patProgram(1, 0x1000)})
	e.emitPMT(1, 0x1000, 1, 0, 1, 0x100, []*PMTElementaryStream{es(StreamTypeH264Video, 0x100)})
	e.emitPMT(1, 0x1000, 1, 1, 1, 0x100, []*PMTElementaryStream{es(StreamTypeADTS, 0x101)})

	// Version 2: section 0 only, section 1 lost.
	e.emitPMT(1, 0x1000, 2, 0, 1, 0x100, []*PMTElementaryStream{es(StreamTypeH264Video, 0x100)})
	// Full retransmission of version 2 eventually completes the table.
	e.emitPMT(1, 0x1000, 2, 0, 1, 0x100, []*PMTElementaryStream{es(StreamTypeH264Video, 0x100)})
	e.emitPMT(1, 0x1000, 2, 1, 1, 0x100, []*PMTElementaryStream{es(StreamTypeAC3Audio, 0x104)})

	events, parseErrors := runDemuxer(e.buf.Bytes())
	require.Empty(t, parseErrors)
	require.Len(t, events, 1)
	assert.Equal(t, uint8(1), events[0].PreviousVersion)
	assert.Equal(t, uint8(2), events[0].Version)
	require.Len(t, events[0].Changes, 2)
	assert.Equal(t, uint16(0x104), events[0].Changes[0].PID)
	assert.Equal(t, ProgramMapChangeKindAdded, events[0].Changes[0].Kind)
	assert.Equal(t, uint16(0x101), events[0].Changes[1].PID)
	assert.Equal(t, ProgramMapChangeKindRemoved, events[0].Changes[1].Kind)
}

func TestProgramMapChangeKindString(t *testing.T) {
	assert.Equal(t, "added", ProgramMapChangeKindAdded.String())
	assert.Equal(t, "removed", ProgramMapChangeKindRemoved.String())
	assert.Equal(t, "type_changed", ProgramMapChangeKindTypeChanged.String())
	assert.Equal(t, "unknown", ProgramMapChangeKind(42).String())
}

// resetUnlocked must drop the queued payload of one PID without touching the
// other accumulators, so a corrupt PSI table cannot contaminate the next one.
func TestPacketPoolResetUnlocked(t *testing.T) {
	pool := newPacketPool(newProgramMap())
	pool.addUnlocked(&Packet{Header: PacketHeader{HasPayload: true, PID: PIDPAT, PayloadUnitStartIndicator: true}, Payload: []byte{0}})
	pool.addUnlocked(&Packet{Header: PacketHeader{HasPayload: true, PID: 0x1000, PayloadUnitStartIndicator: true}, Payload: []byte{1}})

	pool.resetUnlocked(PIDPAT)

	assert.Empty(t, pool.b[uint32(PIDPAT)].q)
	require.Contains(t, pool.b, uint32(0x1000))
	assert.Len(t, pool.b[uint32(0x1000)].q, 1)

	// Resetting an unknown PID is a no-op.
	assert.NotPanics(t, func() { pool.resetUnlocked(0x1234) })
}

// Rewind resets the monitor: the same first table version establishes a fresh
// baseline instead of being reported as a change.
func TestDemuxerRewindResetsProgramMapMonitor(t *testing.T) {
	e := newTSEmitter()
	e.emitPAT(1, 5, 0, 0, []*PATProgram{patProgram(1, 0x1000)})
	e.emitPMT(1, 0x1000, 9, 0, 0, 0x100, []*PMTElementaryStream{es(StreamTypeADTS, 0x101)})
	// A changed version that would normally emit an event.
	e.emitPMT(1, 0x1000, 10, 0, 0, 0x100, []*PMTElementaryStream{es(StreamTypeAC3Audio, 0x101)})

	collect := func(dmx *Demuxer) ([]*ProgramMapEvent, []error) {
		var events []*ProgramMapEvent
		var parseErrors []error
		for {
			d, err := dmx.NextData()
			if err != nil {
				if err == ErrNoMorePackets {
					return events, parseErrors
				}
				parseErrors = append(parseErrors, err)
				continue
			}
			if d.ProgramMapEvent != nil {
				events = append(events, d.ProgramMapEvent)
			}
		}
	}

	dmx := NewDemuxer(context.Background(), bytes.NewReader(e.buf.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))
	firstRun, parseErrors := collect(dmx)
	require.Empty(t, parseErrors)
	require.Len(t, firstRun, 1)
	assert.Equal(t, uint8(9), firstRun[0].PreviousVersion)
	assert.Equal(t, uint8(10), firstRun[0].Version)

	// Rewind and re-read the exact same stream.
	_, err := dmx.Rewind()
	require.NoError(t, err)
	secondRun, parseErrors := collect(dmx)
	require.Empty(t, parseErrors)
	require.Len(t, secondRun, 1)
	// Same result again: baseline versions 5/9 are not reported after the reset.
	assert.Equal(t, uint8(9), secondRun[0].PreviousVersion)
	assert.Equal(t, uint8(10), secondRun[0].Version)
}
