package main

import (
	"context"
	"errors"
	"testing"

	p4v1 "github.com/p4lang/p4runtime/go/p4/v1"
)

const testDigestID = uint32(0x17000001)

func TestDecodeDigestList(t *testing.T) {
	list := &p4v1.DigestList{
		DigestId: testDigestID,
		ListId:   99,
		Data: []*p4v1.P4Data{
			testDigestData([]byte{1}, []byte{1}),
			testDigestData([]byte{2, 0, 0, 0, 0, 2}, []byte{4}),
		},
	}
	samples, err := decodeDigestList(list, testDigestID)
	if err != nil {
		t.Fatal(err)
	}
	want := []learnSample{
		{mac: mustTestMAC(t, "00:00:00:00:00:01"), port: 1},
		{mac: mustTestMAC(t, "02:00:00:00:00:02"), port: 4},
	}
	if len(samples) != len(want) {
		t.Fatalf("decoded %d samples, want %d", len(samples), len(want))
	}
	for index := range want {
		if samples[index] != want[index] {
			t.Fatalf("sample %d = %+v, want %+v", index, samples[index], want[index])
		}
	}
}

func TestDecodeDigestListRejectsMalformedData(t *testing.T) {
	valid := testDigestData([]byte{1}, []byte{1})
	wrongMember := &p4v1.P4Data{
		Data: &p4v1.P4Data_Struct{Struct: &p4v1.P4StructLike{
			Members: []*p4v1.P4Data{
				{Data: &p4v1.P4Data_Tuple{Tuple: &p4v1.P4StructLike{}}},
				testBitstring([]byte{1}),
			},
		}},
	}
	wrongPort := testStruct(
		testBitstring([]byte{1}),
		&p4v1.P4Data{Data: &p4v1.P4Data_Tuple{Tuple: &p4v1.P4StructLike{}}},
	)
	tests := []struct {
		name string
		list *p4v1.DigestList
	}{
		{name: "nil"},
		{name: "wrong ID", list: testDigestList(testDigestID+1, valid)},
		{name: "empty", list: testDigestList(testDigestID)},
		{name: "nil member", list: testDigestList(testDigestID, nil)},
		{name: "plain bitstring", list: testDigestList(testDigestID, testBitstring([]byte{1}))},
		{name: "nil struct", list: testDigestList(testDigestID, &p4v1.P4Data{Data: &p4v1.P4Data_Struct{}})},
		{name: "wrong field count", list: testDigestList(testDigestID, testStruct(testBitstring([]byte{1})))},
		{name: "wrong source type", list: testDigestList(testDigestID, wrongMember)},
		{name: "wrong port type", list: testDigestList(testDigestID, wrongPort)},
		{name: "empty source", list: testDigestList(testDigestID, testDigestData(nil, []byte{1}))},
		{name: "wide source", list: testDigestList(testDigestID, testDigestData([]byte{1, 0, 0, 0, 0, 0, 1}, []byte{1}))},
		{name: "zero source", list: testDigestList(testDigestID, testDigestData([]byte{0}, []byte{1}))},
		{name: "multicast source", list: testDigestList(testDigestID, testDigestData([]byte{1, 0, 0, 0, 0, 1}, []byte{1}))},
		{name: "empty port", list: testDigestList(testDigestID, testDigestData([]byte{1}, nil))},
		{name: "zero port", list: testDigestList(testDigestID, testDigestData([]byte{1}, []byte{0}))},
		{name: "non-bridge port", list: testDigestList(testDigestID, testDigestData([]byte{1}, []byte{5}))},
		{name: "wide port", list: testDigestList(testDigestID, testDigestData([]byte{1}, []byte{2, 0}))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeDigestList(test.list, testDigestID); err == nil {
				t.Fatal("malformed digest was accepted")
			}
		})
	}
}

func TestDigestListAck(t *testing.T) {
	list := &p4v1.DigestList{DigestId: testDigestID, ListId: 1<<40 + 7}
	ack, err := newDigestListAck(list)
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetDigestId() != list.GetDigestId() || ack.GetListId() != list.GetListId() {
		t.Fatalf("ACK = %v, want digest %d list %d", ack, list.GetDigestId(), list.GetListId())
	}
	if _, err := newDigestListAck(nil); err == nil {
		t.Fatal("nil digest list was acknowledged")
	}
}

func TestDigestProcessorValidatesWholeListBeforeLearning(t *testing.T) {
	programmer := &recordingProgrammer{}
	acknowledged := false
	processor := &digestProcessor{
		digestID: testDigestID,
		learner:  newMACLearner(programmer),
		ack: func(context.Context, *p4v1.DigestListAck) error {
			acknowledged = true
			return nil
		},
	}
	list := testDigestList(
		testDigestID,
		testDigestData([]byte{1}, []byte{1}),
		testDigestData([]byte{2}, []byte{5}),
	)
	if err := processor.process(context.Background(), list); err == nil {
		t.Fatal("malformed digest list was accepted")
	}
	if len(programmer.changes) != 0 || acknowledged {
		t.Fatalf("partial digest processing occurred: changes=%v acknowledged=%v", programmer.changes, acknowledged)
	}
}

func TestDigestProcessorLearnsEveryMemberAndAcknowledges(t *testing.T) {
	programmer := &recordingProgrammer{}
	var gotAck *p4v1.DigestListAck
	processor := &digestProcessor{
		digestID: testDigestID,
		learner:  newMACLearner(programmer),
		ack: func(_ context.Context, ack *p4v1.DigestListAck) error {
			gotAck = ack
			return nil
		},
	}
	list := &p4v1.DigestList{
		DigestId: testDigestID,
		ListId:   42,
		Data: []*p4v1.P4Data{
			testDigestData([]byte{1}, []byte{1}),
			testDigestData([]byte{2}, []byte{2}),
		},
	}
	if err := processor.process(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(programmer.changes) != 2 {
		t.Fatalf("learned %d digest members, want 2", len(programmer.changes))
	}
	if gotAck == nil || gotAck.GetDigestId() != testDigestID || gotAck.GetListId() != 42 {
		t.Fatalf("unexpected ACK: %v", gotAck)
	}
}

func TestDigestProcessorDoesNotAcknowledgeFailedLearning(t *testing.T) {
	programmer := &recordingProgrammer{err: errors.New("write failed")}
	acknowledged := false
	processor := &digestProcessor{
		digestID: testDigestID,
		learner:  newMACLearner(programmer),
		ack: func(context.Context, *p4v1.DigestListAck) error {
			acknowledged = true
			return nil
		},
	}
	if err := processor.process(
		context.Background(),
		testDigestList(testDigestID, testDigestData([]byte{1}, []byte{1})),
	); err == nil {
		t.Fatal("learning failure was ignored")
	}
	if acknowledged {
		t.Fatal("failed learning was acknowledged")
	}
}

func TestDigestProcessorPropagatesAcknowledgementFailure(t *testing.T) {
	processor := &digestProcessor{
		digestID: testDigestID,
		learner:  newMACLearner(&recordingProgrammer{}),
		ack: func(context.Context, *p4v1.DigestListAck) error {
			return errors.New("stream send failed")
		},
	}
	err := processor.process(
		context.Background(),
		testDigestList(testDigestID, testDigestData([]byte{1}, []byte{1})),
	)
	if err == nil {
		t.Fatal("acknowledgement failure was ignored")
	}
}

func testDigestList(id uint32, data ...*p4v1.P4Data) *p4v1.DigestList {
	return &p4v1.DigestList{DigestId: id, ListId: 9, Data: data}
}

func testDigestData(mac, port []byte) *p4v1.P4Data {
	return testStruct(testBitstring(mac), testBitstring(port))
}

func testStruct(members ...*p4v1.P4Data) *p4v1.P4Data {
	return &p4v1.P4Data{
		Data: &p4v1.P4Data_Struct{
			Struct: &p4v1.P4StructLike{Members: members},
		},
	}
}

func testBitstring(value []byte) *p4v1.P4Data {
	return &p4v1.P4Data{Data: &p4v1.P4Data_Bitstring{Bitstring: value}}
}
