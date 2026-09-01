package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	p4v1 "github.com/p4lang/p4runtime/go/p4/v1"
	"github.com/zhh2001/p4runtime-go-controller/client"
	"github.com/zhh2001/p4runtime-go-controller/pipeline"
	"google.golang.org/protobuf/proto"
)

const (
	digestQueueSize        = 64
	learningOperationLimit = 5 * time.Second
	learningRollbackLimit  = 2 * time.Second
	learningShutdownLimit  = 8 * time.Second
)

type digestProcessor struct {
	digestID uint32
	learner  *macLearner
	ack      func(context.Context, *p4v1.DigestListAck) error
}

func (processor *digestProcessor) process(
	ctx context.Context,
	list *p4v1.DigestList,
) error {
	samples, err := decodeDigestList(list, processor.digestID)
	if err != nil {
		return err
	}
	for index, sample := range samples {
		if _, err := processor.learner.learn(ctx, sample); err != nil {
			return fmt.Errorf("digest member %d: %w", index, err)
		}
	}
	ack, err := newDigestListAck(list)
	if err != nil {
		return err
	}
	if err := processor.ack(ctx, ack); err != nil {
		return fmt.Errorf("acknowledge digest list %d: %w", list.GetListId(), err)
	}
	return nil
}

func decodeDigestList(
	list *p4v1.DigestList,
	expectedID uint32,
) ([]learnSample, error) {
	if list == nil {
		return nil, errors.New("digest list is nil")
	}
	if list.GetDigestId() != expectedID {
		return nil, fmt.Errorf(
			"expected digest ID %d, got %d",
			expectedID,
			list.GetDigestId(),
		)
	}
	if len(list.GetData()) == 0 {
		return nil, fmt.Errorf("digest list %d is empty", list.GetListId())
	}

	samples := make([]learnSample, 0, len(list.GetData()))
	for index, data := range list.GetData() {
		structure, ok := data.GetData().(*p4v1.P4Data_Struct)
		if !ok || structure.Struct == nil {
			return nil, fmt.Errorf("digest member %d is not a struct", index)
		}
		members := structure.Struct.GetMembers()
		if len(members) != 2 {
			return nil, fmt.Errorf(
				"digest member %d: expected 2 fields, got %d",
				index,
				len(members),
			)
		}
		macData, ok := members[0].GetData().(*p4v1.P4Data_Bitstring)
		if !ok {
			return nil, fmt.Errorf("digest member %d: source MAC is not a bitstring", index)
		}
		portData, ok := members[1].GetData().(*p4v1.P4Data_Bitstring)
		if !ok {
			return nil, fmt.Errorf("digest member %d: ingress port is not a bitstring", index)
		}
		mac, err := decodeMACValue(macData.Bitstring)
		if err != nil {
			return nil, fmt.Errorf("digest member %d source MAC: %w", index, err)
		}
		port, err := decodePortValue(portData.Bitstring)
		if err != nil {
			return nil, fmt.Errorf("digest member %d ingress port: %w", index, err)
		}
		samples = append(samples, learnSample{mac: mac, port: port})
	}
	return samples, nil
}

func newDigestListAck(list *p4v1.DigestList) (*p4v1.DigestListAck, error) {
	if list == nil {
		return nil, errors.New("digest list is nil")
	}
	return &p4v1.DigestListAck{
		DigestId: list.GetDigestId(),
		ListId:   list.GetListId(),
	}, nil
}

type learningService struct {
	stopAccept context.CancelFunc
	unregister func()
	jobs       chan *p4v1.DigestList
	cancelWork context.CancelFunc
	done       chan struct{}
	errors     chan error
}

func startLearningService(
	c *client.Client,
	p *pipeline.Pipeline,
) (*learningService, error) {
	id, err := digestID(p)
	if err != nil {
		return nil, err
	}
	acceptCtx, stopAccept := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(context.Background())
	service := &learningService{
		stopAccept: stopAccept,
		jobs:       make(chan *p4v1.DigestList, digestQueueSize),
		cancelWork: cancelWork,
		done:       make(chan struct{}),
		errors:     make(chan error, 1),
	}
	processor := &digestProcessor{
		digestID: id,
		learner: newMACLearner(&macTableProgrammer{
			client:   c,
			pipeline: p,
		}),
		ack: c.SendDigestAck,
	}
	service.unregister = c.OnDigestList(func(_ context.Context, list *p4v1.DigestList) {
		if list == nil {
			service.fail(errors.New("received a nil digest list"))
			return
		}
		cloned := proto.Clone(list).(*p4v1.DigestList)
		select {
		case service.jobs <- cloned:
		case <-acceptCtx.Done():
		default:
			service.fail(errors.New("digest queue is full"))
		}
	})
	go service.run(workCtx, processor)
	return service, nil
}

func (service *learningService) run(
	ctx context.Context,
	processor *digestProcessor,
) {
	defer close(service.done)
	for list := range service.jobs {
		operationCtx, cancel := context.WithTimeout(ctx, learningOperationLimit)
		err := processor.process(operationCtx, list)
		cancel()
		if err != nil {
			service.fail(err)
			return
		}
	}
}

func (service *learningService) fail(err error) {
	service.stopAccept()
	select {
	case service.errors <- err:
	default:
	}
}

func (service *learningService) close(ctx context.Context) error {
	service.stopAccept()
	service.unregister()
	close(service.jobs)
	select {
	case <-service.done:
		service.cancelWork()
		return nil
	case <-ctx.Done():
		service.cancelWork()
		<-service.done
		return ctx.Err()
	}
}
