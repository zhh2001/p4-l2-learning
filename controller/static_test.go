package main

import (
	"strings"
	"testing"

	p4configv1 "github.com/p4lang/p4runtime/go/p4/config/v1"
	p4v1 "github.com/p4lang/p4runtime/go/p4/v1"
	"github.com/zhh2001/p4runtime-go-controller/pipeline"
	"github.com/zhh2001/p4runtime-go-controller/pre"
	"google.golang.org/protobuf/proto"
)

func TestDesiredFloodGroup(t *testing.T) {
	group := desiredFloodGroup()
	if group.ID != 1 {
		t.Fatalf("group ID = %d, want 1", group.ID)
	}
	want := []pre.Replica{
		{EgressPort: 1},
		{EgressPort: 2},
		{EgressPort: 3},
		{EgressPort: 4},
	}
	if !replicasEqual(group.Replicas, want) {
		t.Fatalf("replicas = %v, want %v", group.Replicas, want)
	}
}

func TestDesiredDigestEntry(t *testing.T) {
	entry := desiredDigestEntry(123)
	if entry.GetDigestId() != 123 {
		t.Fatalf("digest ID = %d, want 123", entry.GetDigestId())
	}
	config := entry.GetConfig()
	if config.GetMaxTimeoutNs() != 0 {
		t.Fatalf("max timeout = %d, want 0", config.GetMaxTimeoutNs())
	}
	if config.GetMaxListSize() != 1 {
		t.Fatalf("max list size = %d, want 1", config.GetMaxListSize())
	}
	if config.GetAckTimeoutNs() != 1_000_000_000 {
		t.Fatalf("ACK timeout = %d, want 1000000000", config.GetAckTimeoutNs())
	}
}

func TestDigestID(t *testing.T) {
	p := newTestPipeline(t, true, []byte("device config"))
	id, err := digestID(p)
	if err != nil {
		t.Fatal(err)
	}
	if id != 0x17000001 {
		t.Fatalf("digest ID = %#x, want %#x", id, uint32(0x17000001))
	}

	p = newTestPipeline(t, false, nil)
	if _, err := digestID(p); err == nil || !strings.Contains(err.Error(), learnDigestName) {
		t.Fatalf("missing digest error = %v", err)
	}
}

func TestDigestEntityAndUpdate(t *testing.T) {
	entry := desiredDigestEntry(99)
	entity := digestEntity(entry)
	if entity.GetDigestEntry() != entry {
		t.Fatal("digest entity does not contain the requested entry")
	}
	update := digestUpdate(p4v1.Update_INSERT, entry)
	if update.GetType() != p4v1.Update_INSERT || update.GetEntity().GetDigestEntry() != entry {
		t.Fatalf("unexpected digest update: %v", update)
	}
}

func TestComparePipelines(t *testing.T) {
	want := newTestPipeline(t, true, []byte("device config"))
	got := newTestPipeline(t, true, []byte("device config"))
	if err := comparePipelines(want, got); err != nil {
		t.Fatal(err)
	}
	if err := comparePipelines(want, newTestPipeline(t, true, []byte("other"))); err == nil {
		t.Fatal("different device configuration was accepted")
	}
	changed := newTestPipeline(t, false, []byte("device config"))
	if err := comparePipelines(want, changed); err == nil {
		t.Fatal("different P4Info was accepted")
	}
}

func TestVerifyFloodGroups(t *testing.T) {
	valid := desiredFloodGroup()
	valid.Replicas[0], valid.Replicas[3] = valid.Replicas[3], valid.Replicas[0]
	if err := verifyFloodGroups([]pre.MulticastGroup{valid}); err != nil {
		t.Fatalf("reordered replicas rejected: %v", err)
	}

	tests := []struct {
		name   string
		groups []pre.MulticastGroup
	}{
		{name: "missing"},
		{name: "extra group", groups: []pre.MulticastGroup{desiredFloodGroup(), {ID: 2}}},
		{name: "wrong group", groups: []pre.MulticastGroup{{ID: 2, Replicas: valid.Replicas}}},
		{name: "missing replica", groups: []pre.MulticastGroup{{ID: 1, Replicas: valid.Replicas[:3]}}},
		{name: "extra replica", groups: []pre.MulticastGroup{{ID: 1, Replicas: append(valid.Replicas, pre.Replica{EgressPort: 5})}}},
		{name: "wrong instance", groups: []pre.MulticastGroup{{ID: 1, Replicas: []pre.Replica{{EgressPort: 1, Instance: 1}, {EgressPort: 2}, {EgressPort: 3}, {EgressPort: 4}}}}},
		{name: "metadata", groups: []pre.MulticastGroup{{ID: 1, Replicas: valid.Replicas, Metadata: []byte{1}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := verifyFloodGroups(test.groups); err == nil {
				t.Fatal("invalid PRE state was accepted")
			}
		})
	}
}

func TestVerifyDigestEntry(t *testing.T) {
	want := desiredDigestEntry(123)
	if err := verifyDigestEntry(proto.Clone(want).(*p4v1.DigestEntry), want); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		entry *p4v1.DigestEntry
	}{
		{name: "missing"},
		{name: "wrong ID", entry: desiredDigestEntry(124)},
		{name: "missing config", entry: &p4v1.DigestEntry{DigestId: 123}},
		{name: "max timeout", entry: desiredDigestEntry(123)},
		{name: "max list size", entry: desiredDigestEntry(123)},
		{name: "ACK timeout", entry: desiredDigestEntry(123)},
	}
	tests[3].entry.Config.MaxTimeoutNs = 1
	tests[4].entry.Config.MaxListSize = 2
	tests[5].entry.Config.AckTimeoutNs = 2
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := verifyDigestEntry(test.entry, want); err == nil {
				t.Fatal("invalid digest state was accepted")
			}
		})
	}
}

func TestVerifyNoLearnedEntries(t *testing.T) {
	if err := verifyNoLearnedEntries("source_location", 10, nil); err != nil {
		t.Fatal(err)
	}
	defaultEntry := &p4v1.TableEntry{TableId: 10, IsDefaultAction: true}
	if err := verifyNoLearnedEntries("source_location", 10, []*p4v1.TableEntry{defaultEntry}); err != nil {
		t.Fatal(err)
	}
	if err := verifyNoLearnedEntries(
		"source_location",
		10,
		[]*p4v1.TableEntry{{TableId: 10}},
	); err == nil {
		t.Fatal("prelearned entry was accepted")
	}
	if err := verifyNoLearnedEntries(
		"source_location",
		10,
		[]*p4v1.TableEntry{{TableId: 11, IsDefaultAction: true}},
	); err == nil {
		t.Fatal("entry from the wrong table was accepted")
	}
}

func replicasEqual(left, right []pre.Replica) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func newTestPipeline(t *testing.T, includeDigest bool, deviceConfig []byte) *pipeline.Pipeline {
	t.Helper()
	info := &p4configv1.P4Info{
		Tables: []*p4configv1.Table{
			{Preamble: &p4configv1.Preamble{Id: 0x02000001, Name: "IngressImpl.source_location", Alias: "source_location"}},
			{Preamble: &p4configv1.Preamble{Id: 0x02000002, Name: "IngressImpl.destination_mac", Alias: "destination_mac"}},
		},
	}
	if includeDigest {
		info.Digests = []*p4configv1.Digest{
			{Preamble: &p4configv1.Preamble{Id: 0x17000001, Name: learnDigestName, Alias: learnDigestName}},
		}
	}
	p, err := pipeline.New(info, deviceConfig)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
