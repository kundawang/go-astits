package astits

import (
	"errors"
	"fmt"
	"sort"
)

// ProgramMapChangeKind describes how a PID changes between two versions of a
// PAT or PMT table.
type ProgramMapChangeKind uint8

const (
	// ProgramMapChangeKindAdded means the PID is referenced by the new table
	// version but was not referenced by the previous one.
	ProgramMapChangeKindAdded ProgramMapChangeKind = iota
	// ProgramMapChangeKindRemoved means the PID was referenced by the previous
	// table version but is no longer referenced by the new one.
	ProgramMapChangeKindRemoved
	// ProgramMapChangeKindTypeChanged means the PID is still referenced but its
	// stream type changed (PMT elementary streams only).
	ProgramMapChangeKindTypeChanged
)

// String returns a human readable representation of the change kind.
func (k ProgramMapChangeKind) String() string {
	switch k {
	case ProgramMapChangeKindAdded:
		return "added"
	case ProgramMapChangeKindRemoved:
		return "removed"
	case ProgramMapChangeKindTypeChanged:
		return "type_changed"
	}
	return "unknown"
}

// ProgramMapChange describes a single PID change between two versions of a
// program specific table.
type ProgramMapChange struct {
	// Kind is the kind of change.
	Kind ProgramMapChangeKind
	// PID is the PID affected by the change. For a PAT table it is the PMT PID
	// of the program, for a PMT table it is an elementary stream PID.
	PID uint16
	// PreviousStreamType is the stream type referenced before the change. It is
	// 0 for added PIDs and for PAT changes, where the concept does not apply.
	PreviousStreamType StreamType
	// StreamType is the stream type referenced after the change. It is 0 for
	// removed PIDs and for PAT changes, where the concept does not apply.
	StreamType StreamType
}

// ProgramMapEvent reports the differences detected between two complete
// versions of a PAT or PMT table. An event is only emitted once a multi-section
// table has been fully reassembled, and retransmissions of the same table
// version never trigger an event.
type ProgramMapEvent struct {
	// TableType is either PSITableTypePAT or PSITableTypePMT.
	TableType string
	// ProgramNumber is the program the event applies to. It is 0 for PAT
	// events, whose changes span every program of the transport stream.
	ProgramNumber uint16
	// TableIDExtension is the table_id_extension of the triggering table: the
	// transport stream id for a PAT, the program number for a PMT.
	TableIDExtension uint16
	// PreviousVersion is the version_number of the previously committed table,
	// or 0xff when no version had been committed yet.
	PreviousVersion uint8
	// Version is the version_number of the triggering table.
	Version uint8
	// Changes is the ordered list of PID changes. Events with an empty change
	// list are not emitted.
	Changes []*ProgramMapChange
	// FirstPacket is the first packet of the earliest section that
	// participated in the completed table, i.e. the trigger position where the
	// new version first appeared.
	FirstPacket *Packet
	// PID is the PID carrying the triggering table (0 for a PAT).
	PID uint16
}

// ErrPSITableVersionConflict is returned when sections carrying different
// version numbers are observed while a multi-section table is being
// reassembled. The partially reassembled table is discarded and reassembly
// restarts with the conflicting section.
var ErrPSITableVersionConflict = errors.New("astits: PSI table version conflict")

const psiNoVersion uint8 = 0xff

// psiTableAssembly accumulates the sections of a single PAT/PMT table until the
// whole table (sections 0..last_section_number) has been received.
type psiTableAssembly struct {
	tableID          PSITableID
	tableIDExtension uint16
	version          uint8
	lastSection      uint8
	sections         map[uint8]*PSISection
	// firstPacket is the first packet of the first received section, i.e. the
	// earliest trigger position observed for this version.
	firstPacket *Packet
	pid         uint16
}

// add feeds a section to the assembly. It returns ErrPSITableVersionConflict
// when the section does not belong to the version currently being reassembled,
// in which case the caller must reset the assembly and restart with this
// section. The assembly is left untouched when a validation error is returned.
func (a *psiTableAssembly) add(s *PSISection, d *DemuxerData) (complete bool, err error) {
	h := s.Syntax.Header

	// A section from another version or another table invalidates the assembly.
	if h.VersionNumber != a.version || h.TableIDExtension != a.tableIDExtension {
		return false, ErrPSITableVersionConflict
	}

	// Section numbers must stay within the bounds announced by the table, and
	// every section of a table has to announce the same last_section_number.
	if h.SectionNumber > h.LastSectionNumber {
		return false, fmt.Errorf("astits: PSI section number %d exceeds last section number %d", h.SectionNumber, h.LastSectionNumber)
	}
	if len(a.sections) > 0 && h.LastSectionNumber != a.lastSection {
		return false, fmt.Errorf("astits: PSI last section number changed from %d to %d within version %d", a.lastSection, h.LastSectionNumber, a.version)
	}

	if _, ok := a.sections[h.SectionNumber]; !ok {
		a.sections[h.SectionNumber] = s
		a.lastSection = h.LastSectionNumber
	}
	if a.firstPacket == nil && d != nil {
		a.firstPacket = d.FirstPacket
	}
	if d != nil {
		a.pid = d.PID
	}

	return uint8(len(a.sections)) == a.lastSection+1, nil
}

// orderedSections returns the reassembled sections ordered by section number.
func (a *psiTableAssembly) orderedSections() []*PSISection {
	numbers := make([]int, 0, len(a.sections))
	for n := range a.sections {
		numbers = append(numbers, int(n))
	}
	sort.Ints(numbers)
	out := make([]*PSISection, 0, len(numbers))
	for _, n := range numbers {
		out = append(out, a.sections[uint8(n)])
	}
	return out
}

// patProgramState is the committed state of a single program as seen through
// the PAT and its PMT.
type patProgramState struct {
	pmtPID     uint16
	pmtVersion uint8
	// streams maps an elementary PID to its committed stream type.
	streams map[uint32]StreamType
}

// programMapMonitor consumes parsed PAT/PMT sections, reassembles multi-section
// tables and emits ProgramMapEvent when the committed program mapping changes.
//
// Waiting and error rules:
//   - A table is only committed once every section from 0 to last_section_number
//     has been received; missing sections keep the previous version active and
//     are silently waited for.
//   - Sections with a version number or table_id_extension that conflicts with
//     the assembly in progress discard the partial assembly and make handleSection
//     return ErrPSITableVersionConflict; reassembly restarts with the new section.
//   - Sections whose section_number exceeds last_section_number are rejected.
//   - Sections failing the CRC32 check are rejected by the PSI parser before
//     reaching the monitor, so a corrupt table can never be committed: the
//     monitor keeps waiting for a valid retransmission of the missing sections.
//   - Sections with current_next_indicator set to 0 describe a future table and
//     are ignored.
//   - Retransmissions of an already committed version produce no event.
//
// The state is bounded: PMT assemblies and program states are only kept for
// programs referenced by the last committed PAT, and are deleted as soon as the
// PAT removes the program.
type programMapMonitor struct {
	// patAssembly is the PAT table currently being reassembled (nil when idle).
	patAssembly *psiTableAssembly
	// pmtAssemblies indexes in-progress PMT tables by program number.
	pmtAssemblies map[uint32]*psiTableAssembly

	// patVersion is the committed PAT version number.
	patVersion uint8
	// programs indexes the committed state by program number.
	programs map[uint32]*patProgramState
}

// newProgramMapMonitor creates a new program map monitor.
func newProgramMapMonitor() *programMapMonitor {
	return &programMapMonitor{
		patVersion:    psiNoVersion,
		pmtAssemblies: make(map[uint32]*psiTableAssembly),
		programs:      make(map[uint32]*patProgramState),
	}
}

// handleSection feeds a parsed PAT/PMT section to the monitor and returns the
// events emitted by committing a complete table, if any.
func (m *programMapMonitor) handleSection(d *DemuxerData) (events []*ProgramMapEvent, err error) {
	if d == nil || d.Section == nil || d.Section.Syntax == nil || d.Section.Syntax.Header == nil {
		return nil, nil
	}
	s := d.Section
	switch d.Section.Header.TableID {
	case PSITableIDPAT:
		// PAT sections are only valid with PAT data.
		if s.Syntax.Data == nil || s.Syntax.Data.PAT == nil {
			return nil, nil
		}
		return m.handlePATSection(s, d)
	case PSITableIDPMT:
		if s.Syntax.Data == nil || s.Syntax.Data.PMT == nil {
			return nil, nil
		}
		return m.handlePMTSection(s, d)
	}
	return nil, nil
}

// feedAssembly pushes a section into an existing assembly, resetting it with
// the section when a version/extension conflict is detected. It returns the
// (possibly recreated) assembly, whether the table is now complete and the
// conflict error if any.
func feedAssembly(a *psiTableAssembly, s *PSISection, d *DemuxerData, tableID PSITableID) (*psiTableAssembly, bool, error) {
	h := s.Syntax.Header

	// Validate bounds before touching any state: an invalid section must not
	// create or mutate an assembly.
	if a != nil && (h.VersionNumber == a.version && h.TableIDExtension == a.tableIDExtension) {
		if err := validateSectionBounds(h, a.lastSection, len(a.sections) > 0); err != nil {
			return a, false, err
		}
	}

	if a == nil {
		if err := validateSectionBounds(h, h.LastSectionNumber, false); err != nil {
			return nil, false, err
		}
		a = &psiTableAssembly{
			tableID:          tableID,
			tableIDExtension: h.TableIDExtension,
			version:          h.VersionNumber,
			lastSection:      h.LastSectionNumber,
			sections:         make(map[uint8]*PSISection),
		}
	}

	complete, err := a.add(s, d)
	if errors.Is(err, ErrPSITableVersionConflict) {
		// Restart the assembly from the conflicting section.
		if err = validateSectionBounds(h, h.LastSectionNumber, false); err != nil {
			return nil, false, err
		}
		a = &psiTableAssembly{
			tableID:          tableID,
			tableIDExtension: h.TableIDExtension,
			version:          h.VersionNumber,
			lastSection:      h.LastSectionNumber,
			sections:         make(map[uint8]*PSISection),
		}
		// The section is now consistent with the fresh assembly.
		if complete, err = a.add(s, d); err != nil {
			return a, false, err
		}
	} else if err != nil {
		return a, false, err
	}
	return a, complete, nil
}

// validateSectionBounds enforces the section_number/last_section_number rules
// of ISO/IEC 13818-1 without mutating any state.
func validateSectionBounds(h *PSISectionSyntaxHeader, expectedLast uint8, checkLast bool) error {
	if h.SectionNumber > h.LastSectionNumber {
		return fmt.Errorf("astits: PSI section number %d exceeds last section number %d", h.SectionNumber, h.LastSectionNumber)
	}
	if checkLast && h.LastSectionNumber != expectedLast {
		return fmt.Errorf("astits: PSI last section number changed from %d to %d within version %d", expectedLast, h.LastSectionNumber, h.VersionNumber)
	}
	return nil
}

func (m *programMapMonitor) handlePATSection(s *PSISection, d *DemuxerData) (events []*ProgramMapEvent, err error) {
	h := s.Syntax.Header

	// Future tables are not actionable yet.
	if !h.CurrentNextIndicator {
		return nil, nil
	}

	var complete bool
	if m.patAssembly, complete, err = feedAssembly(m.patAssembly, s, d, PSITableIDPAT); err != nil {
		return nil, fmt.Errorf("astits: reassembling PAT failed: %w", err)
	}
	if !complete {
		// Wait for the remaining sections before committing anything.
		return nil, nil
	}

	// Retransmission of the committed version: no event, drop the assembly.
	if m.patVersion == m.patAssembly.version {
		m.patAssembly = nil
		return nil, nil
	}

	// Merge the sections into a single program list.
	programs := mergePATSections(m.patAssembly.orderedSections())
	assembly := m.patAssembly
	m.patAssembly = nil

	return m.commitPAT(programs, assembly), nil
}

func (m *programMapMonitor) handlePMTSection(s *PSISection, d *DemuxerData) (events []*ProgramMapEvent, err error) {
	h := s.Syntax.Header
	programNumber := h.TableIDExtension

	// A PMT for a program the current PAT does not reference cannot be
	// committed; dropping it keeps the monitor state bounded by active programs.
	if _, ok := m.programs[uint32(programNumber)]; !ok {
		return nil, nil
	}

	if !h.CurrentNextIndicator {
		return nil, nil
	}

	assembly := m.pmtAssemblies[uint32(programNumber)]
	var complete bool
	if assembly, complete, err = feedAssembly(assembly, s, d, PSITableIDPMT); err != nil {
		return nil, fmt.Errorf("astits: reassembling PMT for program %d failed: %w", programNumber, err)
	}
	m.pmtAssemblies[uint32(programNumber)] = assembly
	if !complete {
		return nil, nil
	}
	delete(m.pmtAssemblies, uint32(programNumber))

	state := m.programs[uint32(programNumber)]

	// Retransmission of the committed version: no event.
	if state.pmtVersion == assembly.version {
		return nil, nil
	}

	pmt := mergePMTSections(assembly.orderedSections())
	return m.commitPMT(state, pmt, assembly), nil
}

// mergePATSections concatenates the program entries of reassembled PAT sections.
func mergePATSections(sections []*PSISection) []*PATProgram {
	var out []*PATProgram
	for _, s := range sections {
		if s.Syntax != nil && s.Syntax.Data != nil && s.Syntax.Data.PAT != nil {
			out = append(out, s.Syntax.Data.PAT.Programs...)
		}
	}
	return out
}

// mergePMTSections combines reassembled PMT sections into a single PMT data.
// Section 0 carries the program-level fields, the elementary streams of every
// section are concatenated in section order.
func mergePMTSections(sections []*PSISection) *PMTData {
	out := &PMTData{}
	for i, s := range sections {
		pmt := s.Syntax.Data.PMT
		if i == 0 {
			out.ProgramNumber = pmt.ProgramNumber
			out.PCRPID = pmt.PCRPID
			out.ProgramDescriptors = pmt.ProgramDescriptors
		}
		out.ElementaryStreams = append(out.ElementaryStreams, pmt.ElementaryStreams...)
	}
	return out
}

// commitPAT diffs a freshly reassembled PAT against the committed state and
// returns an event per program whose PMT PID was added, removed or replaced.
func (m *programMapMonitor) commitPAT(programs []*PATProgram, assembly *psiTableAssembly) []*ProgramMapEvent {
	// Build the new program -> PMT PID mapping, ignoring the NIT entry (0).
	next := make(map[uint32]uint16, len(programs))
	for _, p := range programs {
		if p.ProgramNumber == 0 {
			continue
		}
		next[uint32(p.ProgramNumber)] = p.ProgramMapID
	}

	// Deterministic event ordering across map iterations.
	numbers := make([]uint32, 0, len(programs))
	seen := make(map[uint32]bool)
	for _, p := range programs {
		if p.ProgramNumber == 0 || seen[uint32(p.ProgramNumber)] {
			continue
		}
		seen[uint32(p.ProgramNumber)] = true
		numbers = append(numbers, uint32(p.ProgramNumber))
	}
	for n := range m.programs {
		if !seen[n] {
			numbers = append(numbers, n)
		}
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })

	var events []*ProgramMapEvent
	// The first committed PAT has no previous version: it only establishes the
	// baseline and must not report the initial programs as additions.
	baseline := m.patVersion == psiNoVersion
	for _, n := range numbers {
		programNumber := uint16(n)
		nextPID, nextOK := next[n]
		state, prevOK := m.programs[n]

		var changes []*ProgramMapChange
		switch {
		case nextOK && !prevOK:
			if !baseline {
				// New program advertised by the PAT.
				changes = []*ProgramMapChange{{Kind: ProgramMapChangeKindAdded, PID: nextPID}}
			}
			m.programs[n] = &patProgramState{
				pmtPID:     nextPID,
				pmtVersion: psiNoVersion,
				streams:    make(map[uint32]StreamType),
			}
		case !nextOK && prevOK:
			// Program removed by the PAT: report its PMT PID and every
			// elementary PID it still owned, then drop all of its state.
			changes = append(changes, &ProgramMapChange{Kind: ProgramMapChangeKindRemoved, PID: state.pmtPID})
			changes = append(changes, removedStreamChanges(state.streams)...)
			delete(m.programs, n)
			delete(m.pmtAssemblies, n)
		case nextOK && prevOK && nextPID != state.pmtPID:
			// PMT PID reused for the same program number.
			changes = []*ProgramMapChange{
				{Kind: ProgramMapChangeKindRemoved, PID: state.pmtPID},
				{Kind: ProgramMapChangeKindAdded, PID: nextPID},
			}
			state.pmtPID = nextPID
			// The new PMT starts from an unknown version again.
			state.pmtVersion = psiNoVersion
			state.streams = make(map[uint32]StreamType)
			delete(m.pmtAssemblies, n)
		}

		if len(changes) > 0 {
			events = append(events, &ProgramMapEvent{
				TableType:        PSITableTypePAT,
				ProgramNumber:    programNumber,
				TableIDExtension: assembly.tableIDExtension,
				PreviousVersion:  m.patVersion,
				Version:          assembly.version,
				Changes:          changes,
				FirstPacket:      assembly.firstPacket,
				PID:              assembly.pid,
			})
		}
	}

	m.patVersion = assembly.version
	return events
}

// commitPMT diffs a freshly reassembled PMT against the committed elementary
// streams of the program.
func (m *programMapMonitor) commitPMT(state *patProgramState, pmt *PMTData, assembly *psiTableAssembly) []*ProgramMapEvent {
	// Build the new PID -> stream type mapping. If a PID appears twice in a
	// table the last occurrence wins, mirroring how a receiver would interpret
	// the stream.
	next := make(map[uint32]StreamType, len(pmt.ElementaryStreams))
	for _, es := range pmt.ElementaryStreams {
		next[uint32(es.ElementaryPID)] = es.StreamType
	}

	var changes []*ProgramMapChange

	// Added and type-changed PIDs, in the order of the new table.
	for _, es := range pmt.ElementaryStreams {
		prevType, ok := state.streams[uint32(es.ElementaryPID)]
		switch {
		case !ok:
			changes = append(changes, &ProgramMapChange{
				Kind:       ProgramMapChangeKindAdded,
				PID:        es.ElementaryPID,
				StreamType: es.StreamType,
			})
		case prevType != es.StreamType:
			changes = append(changes, &ProgramMapChange{
				Kind:               ProgramMapChangeKindTypeChanged,
				PID:                es.ElementaryPID,
				PreviousStreamType: prevType,
				StreamType:         es.StreamType,
			})
		}
	}

	// Removed PIDs, in deterministic PID order.
	var removed []uint32
	for pid := range state.streams {
		if _, ok := next[pid]; !ok {
			removed = append(removed, pid)
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	for _, pid := range removed {
		changes = append(changes, &ProgramMapChange{
			Kind:               ProgramMapChangeKindRemoved,
			PID:                uint16(pid),
			PreviousStreamType: state.streams[pid],
		})
	}

	// Commit the new baseline even when nothing changed at the PID level: a new
	// version with identical PIDs must not be reported again on retransmission.
	previousVersion := state.pmtVersion
	state.pmtVersion = assembly.version
	state.streams = next

	// The first PMT version observed for a program only establishes the
	// baseline and must not report its initial elementary streams.
	if previousVersion == psiNoVersion || len(changes) == 0 {
		return nil
	}
	return []*ProgramMapEvent{{
		TableType:        PSITableTypePMT,
		ProgramNumber:    pmt.ProgramNumber,
		TableIDExtension: assembly.tableIDExtension,
		PreviousVersion:  previousVersion,
		Version:          assembly.version,
		Changes:          changes,
		FirstPacket:      assembly.firstPacket,
		PID:              assembly.pid,
	}}
}

// removedStreamChanges builds removed changes for every elementary PID of a
// program that is being deleted.
func removedStreamChanges(streams map[uint32]StreamType) []*ProgramMapChange {
	pids := make([]uint32, 0, len(streams))
	for pid := range streams {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })

	out := make([]*ProgramMapChange, 0, len(pids))
	for _, pid := range pids {
		out = append(out, &ProgramMapChange{
			Kind:               ProgramMapChangeKindRemoved,
			PID:                uint16(pid),
			PreviousStreamType: streams[pid],
		})
	}
	return out
}
