package main

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	p4v1 "github.com/p4lang/p4runtime/go/p4/v1"
	"github.com/zhh2001/p4runtime-go-controller/client"
	"github.com/zhh2001/p4runtime-go-controller/pipeline"
	"github.com/zhh2001/p4runtime-go-controller/pre"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	floodGroupID      = uint32(1)
	learnDigestName   = "mac_learn_digest_t"
	digestMaxTimeout  = int64(0)
	digestMaxListSize = int32(1)
	digestAckTimeout  = int64(1_000_000_000)
)

var learnedTableNames = []string{"source_location", "destination_mac"}

func desiredFloodGroup() pre.MulticastGroup {
	return pre.MulticastGroup{
		ID: floodGroupID,
		Replicas: []pre.Replica{
			{EgressPort: 1},
			{EgressPort: 2},
			{EgressPort: 3},
			{EgressPort: 4},
		},
	}
}

func desiredDigestEntry(digestID uint32) *p4v1.DigestEntry {
	return &p4v1.DigestEntry{
		DigestId: digestID,
		Config: &p4v1.DigestEntry_Config{
			MaxTimeoutNs: digestMaxTimeout,
			MaxListSize:  digestMaxListSize,
			AckTimeoutNs: digestAckTimeout,
		},
	}
}

func digestID(p *pipeline.Pipeline) (uint32, error) {
	digest, ok := p.Digest(learnDigestName)
	if !ok || digest.ID == 0 {
		return 0, fmt.Errorf("P4Info digest %q not found", learnDigestName)
	}
	return digest.ID, nil
}

func digestEntity(entry *p4v1.DigestEntry) *p4v1.Entity {
	return &p4v1.Entity{
		Entity: &p4v1.Entity_DigestEntry{DigestEntry: entry},
	}
}

func digestUpdate(kind client.UpdateType, entry *p4v1.DigestEntry) *p4v1.Update {
	return &p4v1.Update{Type: kind, Entity: digestEntity(entry)}
}

func configureStaticState(
	ctx context.Context,
	c *client.Client,
	p *pipeline.Pipeline,
) error {
	_, err := c.SetPipeline(ctx, p, client.SetPipelineOptions{
		Action:     client.PipelineVerifyAndCommit,
		NoFallback: true,
	})
	if err != nil {
		return fmt.Errorf("install pipeline: %w", err)
	}
	if err := configureFloodGroup(ctx, c); err != nil {
		return err
	}
	if err := configureDigest(ctx, c, p); err != nil {
		return err
	}
	return verifyStaticState(ctx, c, p, true)
}

func configureFloodGroup(ctx context.Context, c *client.Client) error {
	writer, err := pre.NewWriter(c)
	if err != nil {
		return err
	}
	groups, err := writer.ReadMulticastGroups(ctx, 0)
	if err != nil {
		return fmt.Errorf("read PRE groups: %w", err)
	}
	found := false
	for _, group := range groups {
		if group.ID == floodGroupID {
			found = true
			break
		}
	}
	if found {
		err = writer.ModifyMulticastGroup(ctx, desiredFloodGroup())
	} else {
		err = writer.InsertMulticastGroup(ctx, desiredFloodGroup())
	}
	if err != nil {
		return fmt.Errorf("configure PRE group: %w", err)
	}
	return nil
}

func configureDigest(
	ctx context.Context,
	c *client.Client,
	p *pipeline.Pipeline,
) error {
	id, err := digestID(p)
	if err != nil {
		return err
	}
	entry := desiredDigestEntry(id)
	current, err := readDigestEntry(ctx, c, id)
	if err != nil {
		return fmt.Errorf("read digest configuration: %w", err)
	}
	kind := client.UpdateInsert
	if current != nil {
		kind = client.UpdateModify
	}
	if err := c.Write(ctx, client.WriteOptions{}, digestUpdate(kind, entry)); err != nil {
		return fmt.Errorf("configure digest: %w", err)
	}
	return nil
}

func readDigestEntry(
	ctx context.Context,
	c *client.Client,
	id uint32,
) (*p4v1.DigestEntry, error) {
	entities, err := c.Read(ctx, digestEntity(&p4v1.DigestEntry{DigestId: id}))
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []*p4v1.DigestEntry
	for _, entity := range entities {
		if entry := entity.GetDigestEntry(); entry != nil {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) != 1 {
		return nil, fmt.Errorf("digest %d read returned %d entries", id, len(entries))
	}
	return entries[0], nil
}

func verifyStaticState(
	ctx context.Context,
	c *client.Client,
	want *pipeline.Pipeline,
	requireEmptyTables bool,
) error {
	got, err := c.GetPipeline(ctx)
	if err != nil {
		return fmt.Errorf("read pipeline: %w", err)
	}
	if err := comparePipelines(want, got); err != nil {
		return err
	}

	writer, err := pre.NewWriter(c)
	if err != nil {
		return err
	}
	groups, err := writer.ReadMulticastGroups(ctx, 0)
	if err != nil {
		return fmt.Errorf("read PRE groups: %w", err)
	}
	if err := verifyFloodGroups(groups); err != nil {
		return err
	}

	id, err := digestID(want)
	if err != nil {
		return err
	}
	entry, err := readDigestEntry(ctx, c, id)
	if err != nil {
		return fmt.Errorf("read digest configuration: %w", err)
	}
	if err := verifyDigestEntry(entry, desiredDigestEntry(id)); err != nil {
		return err
	}

	if !requireEmptyTables {
		return nil
	}
	for _, name := range learnedTableNames {
		table, ok := want.Table(name)
		if !ok {
			return fmt.Errorf("P4Info table %q not found", name)
		}
		entries, err := c.ReadTableEntries(ctx, table.ID)
		if err != nil {
			return fmt.Errorf("read table %s: %w", name, err)
		}
		if err := verifyNoLearnedEntries(name, table.ID, entries); err != nil {
			return err
		}
	}
	return nil
}

func comparePipelines(want, got *pipeline.Pipeline) error {
	if want == nil || got == nil {
		return fmt.Errorf("pipeline readback is missing")
	}
	if !proto.Equal(want.Info(), got.Info()) {
		return fmt.Errorf("P4Info readback differs from requested pipeline")
	}
	if !bytes.Equal(want.DeviceConfig(), got.DeviceConfig()) {
		return fmt.Errorf("device configuration readback differs from requested pipeline")
	}
	return nil
}

func verifyFloodGroups(groups []pre.MulticastGroup) error {
	if len(groups) != 1 {
		return fmt.Errorf("expected one PRE group, got %d", len(groups))
	}
	want := desiredFloodGroup()
	got := groups[0]
	if got.ID != want.ID {
		return fmt.Errorf("expected PRE group %d, got %d", want.ID, got.ID)
	}
	if len(got.Metadata) != 0 {
		return fmt.Errorf("PRE group %d has unexpected metadata", got.ID)
	}
	wantReplicas := sortedReplicas(want.Replicas)
	gotReplicas := sortedReplicas(got.Replicas)
	if len(gotReplicas) != len(wantReplicas) {
		return fmt.Errorf(
			"PRE group %d: expected %d replicas, got %d",
			got.ID,
			len(wantReplicas),
			len(gotReplicas),
		)
	}
	for index := range wantReplicas {
		if gotReplicas[index] != wantReplicas[index] {
			return fmt.Errorf(
				"PRE group %d: expected replicas %v, got %v",
				got.ID,
				wantReplicas,
				gotReplicas,
			)
		}
	}
	return nil
}

func sortedReplicas(replicas []pre.Replica) []pre.Replica {
	sorted := append([]pre.Replica(nil), replicas...)
	sort.Slice(sorted, func(left, right int) bool {
		if sorted[left].EgressPort != sorted[right].EgressPort {
			return sorted[left].EgressPort < sorted[right].EgressPort
		}
		return sorted[left].Instance < sorted[right].Instance
	})
	return sorted
}

func verifyDigestEntry(got, want *p4v1.DigestEntry) error {
	if got == nil {
		return fmt.Errorf("digest configuration is missing")
	}
	if !proto.Equal(got, want) {
		return fmt.Errorf("digest configuration mismatch: expected %v, got %v", want, got)
	}
	return nil
}

func verifyNoLearnedEntries(
	tableName string,
	tableID uint32,
	entries []*p4v1.TableEntry,
) error {
	for _, entry := range entries {
		if entry.GetTableId() != tableID {
			return fmt.Errorf(
				"table %s read returned table ID %d",
				tableName,
				entry.GetTableId(),
			)
		}
		if !entry.GetIsDefaultAction() {
			return fmt.Errorf("table %s contains a prelearned entry", tableName)
		}
	}
	return nil
}
