package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"

	p4v1 "github.com/p4lang/p4runtime/go/p4/v1"
	"github.com/zhh2001/p4runtime-go-controller/client"
	"github.com/zhh2001/p4runtime-go-controller/codec"
	"github.com/zhh2001/p4runtime-go-controller/pipeline"
	"github.com/zhh2001/p4runtime-go-controller/tableentry"
	"google.golang.org/protobuf/proto"
)

const (
	macBitwidth  = 48
	portBitwidth = 9
)

type macAddress [6]byte

func (mac macAddress) String() string {
	return net.HardwareAddr(mac[:]).String()
}

type learnSample struct {
	mac  macAddress
	port uint32
}

func parseMACAddress(value string) (macAddress, error) {
	parsed, err := net.ParseMAC(value)
	if err != nil || len(parsed) != len(macAddress{}) {
		return macAddress{}, fmt.Errorf("invalid MAC address %q", value)
	}
	var mac macAddress
	copy(mac[:], parsed)
	if err := validateLearnSample(learnSample{mac: mac, port: 1}); err != nil {
		return macAddress{}, err
	}
	return mac, nil
}

func validBridgePort(port uint32) bool {
	return port >= 1 && port <= 4
}

func validateLearnSample(sample learnSample) error {
	if sample.mac == (macAddress{}) {
		return errors.New("source MAC must not be zero")
	}
	if sample.mac[0]&1 != 0 {
		return fmt.Errorf("source MAC %s is multicast", sample.mac)
	}
	if !validBridgePort(sample.port) {
		return fmt.Errorf("ingress port %d is outside bridge ports 1-4", sample.port)
	}
	return nil
}

type learningKind uint8

const (
	learningUnchanged learningKind = iota
	learningNew
	learningMoved
)

type learningChange struct {
	kind         learningKind
	sample       learnSample
	previousPort uint32
}

type macProgrammer interface {
	apply(context.Context, learningChange) error
}

type macLearner struct {
	ports      map[macAddress]uint32
	programmer macProgrammer
}

func newMACLearner(programmer macProgrammer) *macLearner {
	return &macLearner{
		ports:      make(map[macAddress]uint32),
		programmer: programmer,
	}
}

func (learner *macLearner) learn(
	ctx context.Context,
	sample learnSample,
) (learningChange, error) {
	if err := validateLearnSample(sample); err != nil {
		return learningChange{}, err
	}
	previousPort, exists := learner.ports[sample.mac]
	if exists && previousPort == sample.port {
		return learningChange{kind: learningUnchanged, sample: sample}, nil
	}

	change := learningChange{kind: learningNew, sample: sample}
	if exists {
		change.kind = learningMoved
		change.previousPort = previousPort
	}
	if learner.programmer == nil {
		return learningChange{}, errors.New("MAC programmer is missing")
	}
	if err := learner.programmer.apply(ctx, change); err != nil {
		return learningChange{}, err
	}
	learner.ports[sample.mac] = sample.port
	return change, nil
}

type tableReadWriter interface {
	WriteTableEntry(context.Context, client.UpdateType, *p4v1.TableEntry) error
	ReadTableEntries(context.Context, uint32) ([]*p4v1.TableEntry, error)
}

type tableReader interface {
	ReadTableEntries(context.Context, uint32) ([]*p4v1.TableEntry, error)
}

type macTableProgrammer struct {
	client   tableReadWriter
	pipeline *pipeline.Pipeline
}

type tableOperation struct {
	kind  client.UpdateType
	entry *p4v1.TableEntry
}

type programStep struct {
	forward  tableOperation
	rollback tableOperation
}

func (programmer *macTableProgrammer) apply(
	ctx context.Context,
	change learningChange,
) error {
	steps, err := programSteps(programmer.pipeline, change)
	if err != nil {
		return err
	}
	completed := 0
	for index, step := range steps {
		if err := writeTableOperation(ctx, programmer.client, step.forward); err != nil {
			cause := fmt.Errorf("program learning step %d: %w", index+1, err)
			return rollbackProgram(programmer.client, steps[:completed], cause)
		}
		completed++
	}
	if err := verifyLearnedMAC(ctx, programmer.client, programmer.pipeline, change.sample); err != nil {
		return rollbackProgram(
			programmer.client,
			steps,
			fmt.Errorf("verify learned MAC: %w", err),
		)
	}
	return nil
}

func programSteps(
	p *pipeline.Pipeline,
	change learningChange,
) ([]programStep, error) {
	if err := validateLearnSample(change.sample); err != nil {
		return nil, err
	}
	destinationNew, err := destinationEntry(p, change.sample)
	if err != nil {
		return nil, err
	}
	sourceNew, err := sourceLocationEntry(p, change.sample)
	if err != nil {
		return nil, err
	}

	switch change.kind {
	case learningNew:
		if change.previousPort != 0 {
			return nil, errors.New("new MAC change has a previous port")
		}
		return []programStep{
			{
				forward:  tableOperation{client.UpdateInsert, destinationNew},
				rollback: tableOperation{client.UpdateDelete, keyOnly(destinationNew)},
			},
			{
				forward:  tableOperation{client.UpdateInsert, sourceNew},
				rollback: tableOperation{client.UpdateDelete, keyOnly(sourceNew)},
			},
		}, nil
	case learningMoved:
		oldSample := learnSample{mac: change.sample.mac, port: change.previousPort}
		if err := validateLearnSample(oldSample); err != nil {
			return nil, fmt.Errorf("invalid previous location: %w", err)
		}
		if oldSample.port == change.sample.port {
			return nil, errors.New("MAC move has identical old and new ports")
		}
		destinationOld, err := destinationEntry(p, oldSample)
		if err != nil {
			return nil, err
		}
		sourceOld, err := sourceLocationEntry(p, oldSample)
		if err != nil {
			return nil, err
		}
		return []programStep{
			{
				forward:  tableOperation{client.UpdateModify, destinationNew},
				rollback: tableOperation{client.UpdateModify, destinationOld},
			},
			{
				forward:  tableOperation{client.UpdateDelete, keyOnly(sourceOld)},
				rollback: tableOperation{client.UpdateInsert, sourceOld},
			},
			{
				forward:  tableOperation{client.UpdateInsert, sourceNew},
				rollback: tableOperation{client.UpdateDelete, keyOnly(sourceNew)},
			},
		}, nil
	case learningUnchanged:
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown learning change kind %d", change.kind)
	}
}

func writeTableOperation(
	ctx context.Context,
	writer tableReadWriter,
	operation tableOperation,
) error {
	return writer.WriteTableEntry(ctx, operation.kind, operation.entry)
}

func rollbackProgram(
	writer tableReadWriter,
	completed []programStep,
	cause error,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), learningRollbackLimit)
	defer cancel()
	errorsFound := []error{cause}
	for index := len(completed) - 1; index >= 0; index-- {
		if err := writeTableOperation(ctx, writer, completed[index].rollback); err != nil {
			errorsFound = append(
				errorsFound,
				fmt.Errorf("rollback learning step %d: %w", index+1, err),
			)
		}
	}
	return errors.Join(errorsFound...)
}

func sourceLocationEntry(
	p *pipeline.Pipeline,
	sample learnSample,
) (*p4v1.TableEntry, error) {
	if err := validateLearnSample(sample); err != nil {
		return nil, err
	}
	port, err := codec.EncodeUint(uint64(sample.port), portBitwidth)
	if err != nil {
		return nil, err
	}
	return tableentry.NewBuilder(p, "source_location").
		Match("src_mac", tableentry.Exact(sample.mac[:])).
		Match("ingress_port", tableentry.Exact(port)).
		Action("source_known").
		Build()
}

func destinationEntry(
	p *pipeline.Pipeline,
	sample learnSample,
) (*p4v1.TableEntry, error) {
	if err := validateLearnSample(sample); err != nil {
		return nil, err
	}
	port, err := codec.EncodeUint(uint64(sample.port), portBitwidth)
	if err != nil {
		return nil, err
	}
	return tableentry.NewBuilder(p, "destination_mac").
		Match("dst_mac", tableentry.Exact(sample.mac[:])).
		Action("forward", tableentry.Param("port", port)).
		Build()
}

func keyOnly(entry *p4v1.TableEntry) *p4v1.TableEntry {
	key := &p4v1.TableEntry{
		TableId:         entry.GetTableId(),
		Priority:        entry.GetPriority(),
		IsDefaultAction: entry.GetIsDefaultAction(),
	}
	for _, match := range entry.GetMatch() {
		key.Match = append(key.Match, proto.Clone(match).(*p4v1.FieldMatch))
	}
	return key
}

type learnedMACState struct {
	sourcePorts      []uint32
	destinationPorts []uint32
}

func verifyLearnedMAC(
	ctx context.Context,
	reader tableReader,
	p *pipeline.Pipeline,
	want learnSample,
) error {
	state, err := readLearnedMACState(ctx, reader, p, want.mac)
	if err != nil {
		return err
	}
	if len(state.sourcePorts) != 1 || state.sourcePorts[0] != want.port ||
		len(state.destinationPorts) != 1 || state.destinationPorts[0] != want.port {
		return fmt.Errorf(
			"MAC %s: expected source and destination port %d, got source ports %v and destination ports %v",
			want.mac,
			want.port,
			state.sourcePorts,
			state.destinationPorts,
		)
	}
	return nil
}

func readLearnedMACState(
	ctx context.Context,
	reader tableReader,
	p *pipeline.Pipeline,
	mac macAddress,
) (learnedMACState, error) {
	sourceTable, ok := p.Table("source_location")
	if !ok {
		return learnedMACState{}, errors.New("P4Info table \"source_location\" not found")
	}
	destinationTable, ok := p.Table("destination_mac")
	if !ok {
		return learnedMACState{}, errors.New("P4Info table \"destination_mac\" not found")
	}

	sourceEntries, err := reader.ReadTableEntries(ctx, sourceTable.ID)
	if err != nil {
		return learnedMACState{}, fmt.Errorf("read source_location: %w", err)
	}
	destinationEntries, err := reader.ReadTableEntries(ctx, destinationTable.ID)
	if err != nil {
		return learnedMACState{}, fmt.Errorf("read destination_mac: %w", err)
	}

	var state learnedMACState
	for _, entry := range sourceEntries {
		sample, err := decodeSourceLocationEntry(p, entry)
		if err != nil {
			return learnedMACState{}, err
		}
		if sample.mac == mac {
			state.sourcePorts = append(state.sourcePorts, sample.port)
		}
	}
	for _, entry := range destinationEntries {
		sample, err := decodeDestinationEntry(p, entry)
		if err != nil {
			return learnedMACState{}, err
		}
		if sample.mac == mac {
			state.destinationPorts = append(state.destinationPorts, sample.port)
		}
	}
	sort.Slice(state.sourcePorts, func(left, right int) bool {
		return state.sourcePorts[left] < state.sourcePorts[right]
	})
	sort.Slice(state.destinationPorts, func(left, right int) bool {
		return state.destinationPorts[left] < state.destinationPorts[right]
	})
	return state, nil
}

func decodeSourceLocationEntry(
	p *pipeline.Pipeline,
	entry *p4v1.TableEntry,
) (learnSample, error) {
	table, ok := p.Table("source_location")
	if !ok {
		return learnSample{}, errors.New("P4Info table \"source_location\" not found")
	}
	if err := validateTableEntryShape(entry, table.ID, 2); err != nil {
		return learnSample{}, fmt.Errorf("source_location entry: %w", err)
	}
	macValue, err := exactMatchValue(entry, table, "src_mac")
	if err != nil {
		return learnSample{}, fmt.Errorf("source_location entry: %w", err)
	}
	portValue, err := exactMatchValue(entry, table, "ingress_port")
	if err != nil {
		return learnSample{}, fmt.Errorf("source_location entry: %w", err)
	}
	mac, err := decodeMACValue(macValue)
	if err != nil {
		return learnSample{}, fmt.Errorf("source_location src_mac: %w", err)
	}
	port, err := decodePortValue(portValue)
	if err != nil {
		return learnSample{}, fmt.Errorf("source_location ingress_port: %w", err)
	}
	action, ok := p.Action("source_known")
	if !ok {
		return learnSample{}, errors.New("P4Info action \"source_known\" not found")
	}
	direct := entry.GetAction().GetAction()
	if direct == nil || direct.GetActionId() != action.ID || len(direct.GetParams()) != 0 {
		return learnSample{}, errors.New("source_location entry has an unexpected action")
	}
	return learnSample{mac: mac, port: port}, nil
}

func decodeDestinationEntry(
	p *pipeline.Pipeline,
	entry *p4v1.TableEntry,
) (learnSample, error) {
	table, ok := p.Table("destination_mac")
	if !ok {
		return learnSample{}, errors.New("P4Info table \"destination_mac\" not found")
	}
	if err := validateTableEntryShape(entry, table.ID, 1); err != nil {
		return learnSample{}, fmt.Errorf("destination_mac entry: %w", err)
	}
	macValue, err := exactMatchValue(entry, table, "dst_mac")
	if err != nil {
		return learnSample{}, fmt.Errorf("destination_mac entry: %w", err)
	}
	mac, err := decodeMACValue(macValue)
	if err != nil {
		return learnSample{}, fmt.Errorf("destination_mac dst_mac: %w", err)
	}
	action, ok := p.Action("forward")
	if !ok {
		return learnSample{}, errors.New("P4Info action \"forward\" not found")
	}
	portParam, ok := action.Param("port")
	if !ok {
		return learnSample{}, errors.New("P4Info parameter \"forward.port\" not found")
	}
	direct := entry.GetAction().GetAction()
	if direct == nil || direct.GetActionId() != action.ID || len(direct.GetParams()) != 1 {
		return learnSample{}, errors.New("destination_mac entry has an unexpected action")
	}
	parameter := direct.GetParams()[0]
	if parameter.GetParamId() != portParam.ID {
		return learnSample{}, errors.New("destination_mac entry has an unexpected action parameter")
	}
	port, err := decodePortValue(parameter.GetValue())
	if err != nil {
		return learnSample{}, fmt.Errorf("destination_mac forward port: %w", err)
	}
	return learnSample{mac: mac, port: port}, nil
}

func validateTableEntryShape(entry *p4v1.TableEntry, tableID uint32, matches int) error {
	if entry == nil {
		return errors.New("entry is nil")
	}
	if entry.GetTableId() != tableID {
		return fmt.Errorf("expected table ID %d, got %d", tableID, entry.GetTableId())
	}
	if entry.GetIsDefaultAction() || entry.GetPriority() != 0 ||
		entry.GetIdleTimeoutNs() != 0 || len(entry.GetMetadata()) != 0 {
		return errors.New("entry has unexpected attributes")
	}
	if len(entry.GetMatch()) != matches {
		return fmt.Errorf("expected %d match fields, got %d", matches, len(entry.GetMatch()))
	}
	return nil
}

func exactMatchValue(
	entry *p4v1.TableEntry,
	table *pipeline.TableDef,
	name string,
) ([]byte, error) {
	field, ok := table.MatchField(name)
	if !ok {
		return nil, fmt.Errorf("P4Info match field %q not found", name)
	}
	var value []byte
	found := false
	for _, match := range entry.GetMatch() {
		if match.GetFieldId() != field.ID {
			continue
		}
		if found {
			return nil, fmt.Errorf("match field %q is duplicated", name)
		}
		exact := match.GetExact()
		if exact == nil {
			return nil, fmt.Errorf("match field %q is not exact", name)
		}
		value = exact.GetValue()
		found = true
	}
	if !found {
		return nil, fmt.Errorf("match field %q is missing", name)
	}
	return value, nil
}

func decodeMACValue(value []byte) (macAddress, error) {
	integer, err := decodeUnsignedValue(value, macBitwidth)
	if err != nil {
		return macAddress{}, err
	}
	var mac macAddress
	for index := len(mac) - 1; index >= 0; index-- {
		mac[index] = byte(integer)
		integer >>= 8
	}
	if err := validateLearnSample(learnSample{mac: mac, port: 1}); err != nil {
		return macAddress{}, err
	}
	return mac, nil
}

func decodePortValue(value []byte) (uint32, error) {
	integer, err := decodeUnsignedValue(value, portBitwidth)
	if err != nil {
		return 0, err
	}
	port := uint32(integer)
	if !validBridgePort(port) {
		return 0, fmt.Errorf("port %d is outside bridge ports 1-4", port)
	}
	return port, nil
}

func decodeUnsignedValue(value []byte, bitwidth int) (uint64, error) {
	if len(value) == 0 {
		return 0, errors.New("empty bitstring")
	}
	first := 0
	for first < len(value) && value[first] == 0 {
		first++
	}
	significant := value[first:]
	if len(significant) == 0 {
		return 0, nil
	}
	maxBytes := (bitwidth + 7) / 8
	if len(significant) > maxBytes {
		return 0, fmt.Errorf("value exceeds %d bits", bitwidth)
	}
	if leadingBits := bitwidth % 8; len(significant) == maxBytes && leadingBits != 0 {
		validMask := byte((1 << uint(leadingBits)) - 1)
		if significant[0]&^validMask != 0 {
			return 0, fmt.Errorf("value exceeds %d bits", bitwidth)
		}
	}
	return codec.DecodeUint(significant)
}
