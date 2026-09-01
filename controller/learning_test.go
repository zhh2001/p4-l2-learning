package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	p4configv1 "github.com/p4lang/p4runtime/go/p4/config/v1"
	p4v1 "github.com/p4lang/p4runtime/go/p4/v1"
	"github.com/zhh2001/p4runtime-go-controller/client"
	"github.com/zhh2001/p4runtime-go-controller/pipeline"
	"google.golang.org/protobuf/proto"
)

type recordingProgrammer struct {
	changes []learningChange
	err     error
}

func (programmer *recordingProgrammer) apply(
	_ context.Context,
	change learningChange,
) error {
	programmer.changes = append(programmer.changes, change)
	return programmer.err
}

func TestMACLearnerNewRepeatedAndMove(t *testing.T) {
	programmer := &recordingProgrammer{}
	learner := newMACLearner(programmer)
	first := learnSample{mac: mustTestMAC(t, "00:00:00:00:00:01"), port: 1}

	change, err := learner.learn(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if change.kind != learningNew || change.previousPort != 0 {
		t.Fatalf("unexpected first change: %+v", change)
	}
	if learner.ports[first.mac] != 1 || len(programmer.changes) != 1 {
		t.Fatalf("first learning was not committed: ports=%v changes=%v", learner.ports, programmer.changes)
	}

	change, err = learner.learn(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if change.kind != learningUnchanged || len(programmer.changes) != 1 {
		t.Fatalf("same-port learning was not idempotent: %+v changes=%v", change, programmer.changes)
	}

	moved := first
	moved.port = 3
	change, err = learner.learn(context.Background(), moved)
	if err != nil {
		t.Fatal(err)
	}
	if change.kind != learningMoved || change.previousPort != 1 {
		t.Fatalf("unexpected move: %+v", change)
	}
	if learner.ports[first.mac] != 3 || len(programmer.changes) != 2 {
		t.Fatalf("move was not committed: ports=%v changes=%v", learner.ports, programmer.changes)
	}
}

func TestMACLearnerDoesNotCommitFailedProgramming(t *testing.T) {
	programmer := &recordingProgrammer{err: errors.New("write failed")}
	learner := newMACLearner(programmer)
	sample := learnSample{mac: mustTestMAC(t, "00:00:00:00:00:01"), port: 1}
	if _, err := learner.learn(context.Background(), sample); err == nil {
		t.Fatal("programming failure was ignored")
	}
	if _, found := learner.ports[sample.mac]; found {
		t.Fatal("failed learning changed the in-memory state")
	}
}

func TestMACLearnerKeepsOldPortAfterFailedMove(t *testing.T) {
	programmer := &recordingProgrammer{}
	learner := newMACLearner(programmer)
	first := learnSample{mac: mustTestMAC(t, "00:00:00:00:00:01"), port: 1}
	if _, err := learner.learn(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	programmer.err = errors.New("move failed")
	moved := first
	moved.port = 3
	if _, err := learner.learn(context.Background(), moved); err == nil {
		t.Fatal("failed move was accepted")
	}
	if learner.ports[first.mac] != 1 {
		t.Fatalf("failed move changed in-memory port to %d", learner.ports[first.mac])
	}
}

func TestValidateLearnSample(t *testing.T) {
	valid := mustTestMAC(t, "00:00:00:00:00:02")
	tests := []learnSample{
		{port: 1},
		{mac: mustRawMAC(0x01, 0, 0, 0, 0, 1), port: 1},
		{mac: valid, port: 0},
		{mac: valid, port: 5},
	}
	for _, sample := range tests {
		if err := validateLearnSample(sample); err == nil {
			t.Fatalf("invalid sample accepted: %+v", sample)
		}
	}
	if err := validateLearnSample(learnSample{mac: valid, port: 4}); err != nil {
		t.Fatal(err)
	}
}

func TestLearningEntryBuilders(t *testing.T) {
	p := newLearningTestPipeline(t)
	sample := learnSample{mac: mustTestMAC(t, "00:00:00:00:00:01"), port: 3}

	source, err := sourceLocationEntry(p, sample)
	if err != nil {
		t.Fatal(err)
	}
	decodedSource, err := decodeSourceLocationEntry(p, source)
	if err != nil {
		t.Fatal(err)
	}
	if decodedSource != sample {
		t.Fatalf("source entry decoded as %+v, want %+v", decodedSource, sample)
	}
	if got := source.GetMatch()[0].GetExact().GetValue(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("source MAC is not canonically encoded: %x", got)
	}

	destination, err := destinationEntry(p, sample)
	if err != nil {
		t.Fatal(err)
	}
	decodedDestination, err := decodeDestinationEntry(p, destination)
	if err != nil {
		t.Fatal(err)
	}
	if decodedDestination != sample {
		t.Fatalf("destination entry decoded as %+v, want %+v", decodedDestination, sample)
	}
	action := destination.GetAction().GetAction()
	if got := action.GetParams()[0].GetValue(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("destination port is not canonically encoded: %x", got)
	}

	key := keyOnly(source)
	if key.GetAction() != nil || !proto.Equal(key.GetMatch()[0], source.GetMatch()[0]) ||
		!proto.Equal(key.GetMatch()[1], source.GetMatch()[1]) {
		t.Fatalf("unexpected key-only entry: %v", key)
	}
}

func TestProgramStepsNewAndMove(t *testing.T) {
	p := newLearningTestPipeline(t)
	mac := mustTestMAC(t, "00:00:00:00:00:01")
	newChange := learningChange{
		kind:   learningNew,
		sample: learnSample{mac: mac, port: 1},
	}
	steps, err := programSteps(p, newChange)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].forward.kind != client.UpdateInsert ||
		steps[1].forward.kind != client.UpdateInsert {
		t.Fatalf("unexpected new-MAC steps: %+v", steps)
	}
	if _, err := decodeDestinationEntry(p, steps[0].forward.entry); err != nil {
		t.Fatalf("first step is not destination insertion: %v", err)
	}
	if _, err := decodeSourceLocationEntry(p, steps[1].forward.entry); err != nil {
		t.Fatalf("second step is not source insertion: %v", err)
	}

	move := learningChange{
		kind:         learningMoved,
		sample:       learnSample{mac: mac, port: 3},
		previousPort: 1,
	}
	steps, err = programSteps(p, move)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []client.UpdateType{
		client.UpdateModify,
		client.UpdateDelete,
		client.UpdateInsert,
	}
	if len(steps) != len(wantKinds) {
		t.Fatalf("move step count = %d, want %d", len(steps), len(wantKinds))
	}
	for index, want := range wantKinds {
		if steps[index].forward.kind != want {
			t.Fatalf("move step %d kind = %v, want %v", index, steps[index].forward.kind, want)
		}
	}
	oldKey := steps[1].forward.entry
	if oldKey.GetAction() != nil {
		t.Fatal("old source-location delete contains an action")
	}
	oldSource := proto.Clone(steps[1].rollback.entry).(*p4v1.TableEntry)
	decodedOld, err := decodeSourceLocationEntry(p, oldSource)
	if err != nil || decodedOld.port != 1 {
		t.Fatalf("old source rollback entry = %+v, error %v", decodedOld, err)
	}
	decodedNew, err := decodeSourceLocationEntry(p, steps[2].forward.entry)
	if err != nil || decodedNew.port != 3 {
		t.Fatalf("new source entry = %+v, error %v", decodedNew, err)
	}
}

func TestMACProgrammerReadbackAndMove(t *testing.T) {
	p := newLearningTestPipeline(t)
	tables := newMemoryTableClient()
	programmer := &macTableProgrammer{client: tables, pipeline: p}
	mac := mustTestMAC(t, "00:00:00:00:00:01")
	first := learningChange{
		kind:   learningNew,
		sample: learnSample{mac: mac, port: 1},
	}
	if err := programmer.apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := verifyLearnedMAC(context.Background(), tables, p, first.sample); err != nil {
		t.Fatal(err)
	}

	move := learningChange{
		kind:         learningMoved,
		sample:       learnSample{mac: mac, port: 3},
		previousPort: 1,
	}
	if err := programmer.apply(context.Background(), move); err != nil {
		t.Fatal(err)
	}
	if err := verifyLearnedMAC(context.Background(), tables, p, move.sample); err != nil {
		t.Fatal(err)
	}
	state, err := readLearnedMACState(context.Background(), tables, p, mac)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.sourcePorts) != 1 || state.sourcePorts[0] != 3 {
		t.Fatalf("stale source location remains: %+v", state)
	}
}

func TestMACProgrammerRollsBackFailedMove(t *testing.T) {
	p := newLearningTestPipeline(t)
	tables := newMemoryTableClient()
	programmer := &macTableProgrammer{client: tables, pipeline: p}
	mac := mustTestMAC(t, "00:00:00:00:00:01")
	first := learningChange{
		kind:   learningNew,
		sample: learnSample{mac: mac, port: 1},
	}
	if err := programmer.apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	tables.calls = nil
	tables.failAt = 3
	move := learningChange{
		kind:         learningMoved,
		sample:       learnSample{mac: mac, port: 3},
		previousPort: 1,
	}
	if err := programmer.apply(context.Background(), move); err == nil {
		t.Fatal("failed move was accepted")
	}
	if len(tables.calls) != 5 {
		t.Fatalf("move and rollback made %d writes, want 5", len(tables.calls))
	}
	if err := verifyLearnedMAC(context.Background(), tables, p, first.sample); err != nil {
		t.Fatalf("rollback did not restore old state: %v", err)
	}
}

func TestMACProgrammerUsesFreshRollbackContext(t *testing.T) {
	p := newLearningTestPipeline(t)
	tables := newMemoryTableClient()
	ctx, cancel := context.WithCancel(context.Background())
	tables.cancelAt = 2
	tables.cancel = cancel
	programmer := &macTableProgrammer{client: tables, pipeline: p}
	change := learningChange{
		kind: learningNew,
		sample: learnSample{
			mac:  mustTestMAC(t, "00:00:00:00:00:01"),
			port: 1,
		},
	}
	if err := programmer.apply(ctx, change); err == nil {
		t.Fatal("canceled programming was accepted")
	}
	if len(tables.calls) != 3 {
		t.Fatalf("programming and rollback made %d writes, want 3", len(tables.calls))
	}
	for tableID, entries := range tables.entries {
		if len(entries) != 0 {
			t.Fatalf("table %d was not rolled back: %v", tableID, entries)
		}
	}
}

func TestVerifyLearnedMACRejectsStaleSource(t *testing.T) {
	p := newLearningTestPipeline(t)
	tables := newMemoryTableClient()
	mac := mustTestMAC(t, "00:00:00:00:00:01")
	for _, sample := range []learnSample{{mac: mac, port: 1}, {mac: mac, port: 3}} {
		source, err := sourceLocationEntry(p, sample)
		if err != nil {
			t.Fatal(err)
		}
		tables.add(source)
	}
	destination, err := destinationEntry(p, learnSample{mac: mac, port: 3})
	if err != nil {
		t.Fatal(err)
	}
	tables.add(destination)
	if err := verifyLearnedMAC(
		context.Background(),
		tables,
		p,
		learnSample{mac: mac, port: 3},
	); err == nil || !strings.Contains(err.Error(), "source ports [1 3]") {
		t.Fatalf("stale source entry result = %v", err)
	}
}

func TestVerifyMACAbsent(t *testing.T) {
	p := newLearningTestPipeline(t)
	absent := mustRawMAC(0x01, 0, 0x5e, 0, 0, 1)
	tables := newMemoryTableClient()
	if err := verifyMACAbsent(context.Background(), tables, p, absent); err != nil {
		t.Fatal(err)
	}

	present := mustTestMAC(t, "00:00:00:00:00:01")
	destination, err := destinationEntry(p, learnSample{mac: present, port: 2})
	if err != nil {
		t.Fatal(err)
	}
	tables.add(destination)
	if err := verifyMACAbsent(context.Background(), tables, p, present); err == nil ||
		!strings.Contains(err.Error(), "destination ports [2]") {
		t.Fatalf("present MAC absence result = %v", err)
	}

	tables = newMemoryTableClient()
	source, err := sourceLocationEntry(p, learnSample{mac: present, port: 2})
	if err != nil {
		t.Fatal(err)
	}
	tables.add(source)
	if err := verifyMACAbsent(context.Background(), tables, p, present); err == nil ||
		!strings.Contains(err.Error(), "source ports [2]") {
		t.Fatalf("present MAC absence result = %v", err)
	}
}

type recordedTableOperation struct {
	kind  client.UpdateType
	entry *p4v1.TableEntry
}

type memoryTableClient struct {
	entries  map[uint32][]*p4v1.TableEntry
	calls    []recordedTableOperation
	failAt   int
	cancelAt int
	cancel   context.CancelFunc
}

func newMemoryTableClient() *memoryTableClient {
	return &memoryTableClient{entries: make(map[uint32][]*p4v1.TableEntry)}
}

func (tables *memoryTableClient) WriteTableEntry(
	ctx context.Context,
	kind client.UpdateType,
	entry *p4v1.TableEntry,
) error {
	tables.calls = append(tables.calls, recordedTableOperation{
		kind:  kind,
		entry: proto.Clone(entry).(*p4v1.TableEntry),
	})
	if tables.failAt != 0 && len(tables.calls) == tables.failAt {
		return errors.New("injected write failure")
	}
	if tables.cancelAt != 0 && len(tables.calls) == tables.cancelAt {
		tables.cancel()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries := tables.entries[entry.GetTableId()]
	index := -1
	for candidate := range entries {
		if tableKeysEqual(entries[candidate], entry) {
			index = candidate
			break
		}
	}
	switch kind {
	case client.UpdateInsert:
		if index >= 0 {
			return errors.New("entry already exists")
		}
		tables.add(entry)
	case client.UpdateModify:
		if index < 0 {
			return errors.New("entry does not exist")
		}
		entries[index] = proto.Clone(entry).(*p4v1.TableEntry)
		tables.entries[entry.GetTableId()] = entries
	case client.UpdateDelete:
		if index < 0 {
			return errors.New("entry does not exist")
		}
		tables.entries[entry.GetTableId()] = append(entries[:index], entries[index+1:]...)
	default:
		return errors.New("unexpected update type")
	}
	return nil
}

func (tables *memoryTableClient) ReadTableEntries(
	_ context.Context,
	tableID uint32,
) ([]*p4v1.TableEntry, error) {
	entries := tables.entries[tableID]
	result := make([]*p4v1.TableEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, proto.Clone(entry).(*p4v1.TableEntry))
	}
	return result, nil
}

func (tables *memoryTableClient) add(entry *p4v1.TableEntry) {
	id := entry.GetTableId()
	tables.entries[id] = append(
		tables.entries[id],
		proto.Clone(entry).(*p4v1.TableEntry),
	)
}

func tableKeysEqual(left, right *p4v1.TableEntry) bool {
	return proto.Equal(keyOnly(left), keyOnly(right))
}

func mustTestMAC(t *testing.T, value string) macAddress {
	t.Helper()
	mac, err := parseMACAddress(value)
	if err != nil {
		t.Fatal(err)
	}
	return mac
}

func mustRawMAC(bytes ...byte) macAddress {
	var mac macAddress
	copy(mac[:], bytes)
	return mac
}

func newLearningTestPipeline(t *testing.T) *pipeline.Pipeline {
	t.Helper()
	const (
		sourceTableID      = 0x02000001
		destinationTableID = 0x02000002
		learnActionID      = 0x01000001
		forwardActionID    = 0x01000002
	)
	exact := func() *p4configv1.MatchField_MatchType_ {
		return &p4configv1.MatchField_MatchType_{MatchType: p4configv1.MatchField_EXACT}
	}
	info := &p4configv1.P4Info{
		Tables: []*p4configv1.Table{
			{
				Preamble: &p4configv1.Preamble{Id: sourceTableID, Name: "IngressImpl.source_location", Alias: "source_location"},
				MatchFields: []*p4configv1.MatchField{
					{Id: 1, Name: "src_mac", Bitwidth: macBitwidth, Match: exact()},
					{Id: 2, Name: "ingress_port", Bitwidth: portBitwidth, Match: exact()},
				},
				ActionRefs: []*p4configv1.ActionRef{{Id: learnActionID}},
			},
			{
				Preamble: &p4configv1.Preamble{Id: destinationTableID, Name: "IngressImpl.destination_mac", Alias: "destination_mac"},
				MatchFields: []*p4configv1.MatchField{
					{Id: 1, Name: "dst_mac", Bitwidth: macBitwidth, Match: exact()},
				},
				ActionRefs: []*p4configv1.ActionRef{{Id: forwardActionID}},
			},
		},
		Actions: []*p4configv1.Action{
			{
				Preamble: &p4configv1.Preamble{Id: learnActionID, Name: "IngressImpl.source_known", Alias: "source_known"},
			},
			{
				Preamble: &p4configv1.Preamble{Id: forwardActionID, Name: "IngressImpl.forward", Alias: "forward"},
				Params: []*p4configv1.Action_Param{
					{Id: 1, Name: "port", Bitwidth: portBitwidth},
				},
			},
		},
		Digests: []*p4configv1.Digest{
			{Preamble: &p4configv1.Preamble{Id: 0x17000001, Name: learnDigestName, Alias: learnDigestName}},
		},
	}
	p, err := pipeline.New(info, []byte("device config"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
