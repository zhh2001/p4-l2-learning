package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zhh2001/p4runtime-go-controller/client"
	"github.com/zhh2001/p4runtime-go-controller/pipeline"
)

const startupTimeout = 20 * time.Second

type options struct {
	address          string
	deviceID         uint64
	electionID       uint64
	p4infoPath       string
	deviceConfigPath string
	verifyOnly       bool
}

func parseOptions(args []string) (options, error) {
	var cfg options
	flags := flag.NewFlagSet("learning-controller", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.address, "address", "127.0.0.1:50052", "P4Runtime address")
	flags.Uint64Var(&cfg.deviceID, "device-id", 2, "P4Runtime device ID")
	flags.Uint64Var(&cfg.electionID, "election-id", 1, "P4Runtime election ID")
	flags.StringVar(
		&cfg.p4infoPath,
		"p4info",
		"build/learning_switch.p4info.txtpb",
		"P4Info text protobuf",
	)
	flags.StringVar(
		&cfg.deviceConfigPath,
		"device-config",
		"build/learning_switch.json",
		"BMv2 JSON configuration",
	)
	flags.BoolVar(&cfg.verifyOnly, "verify-only", false, "verify static state and exit")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if cfg.address == "" {
		return options{}, errors.New("P4Runtime address must not be empty")
	}
	if cfg.deviceID == 0 {
		return options{}, errors.New("device ID must be non-zero")
	}
	if cfg.electionID == 0 {
		return options{}, errors.New("election ID must be non-zero")
	}
	return cfg, nil
}

func loadPipeline(p4infoPath, deviceConfigPath string) (*pipeline.Pipeline, error) {
	p4info, err := os.ReadFile(p4infoPath)
	if err != nil {
		return nil, fmt.Errorf("read P4Info: %w", err)
	}
	deviceConfig, err := os.ReadFile(deviceConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read device config: %w", err)
	}
	p, err := pipeline.LoadText(p4info, deviceConfig)
	if err != nil {
		return nil, fmt.Errorf("load pipeline: %w", err)
	}
	return p, nil
}

func runController(ctx context.Context, output io.Writer, cfg options) (runErr error) {
	p, err := loadPipeline(cfg.p4infoPath, cfg.deviceConfigPath)
	if err != nil {
		return err
	}

	setupCtx, cancelSetup := context.WithTimeout(ctx, startupTimeout)
	defer cancelSetup()
	c, err := client.Dial(
		setupCtx,
		cfg.address,
		client.WithDeviceID(cfg.deviceID),
		client.WithElectionID(client.ElectionID{Low: cfg.electionID}),
		client.WithInsecure(),
		client.WithArbitrationTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect to P4Runtime: %w", err)
	}
	defer func() {
		if err := c.Close(); runErr == nil && err != nil {
			runErr = fmt.Errorf("close P4Runtime client: %w", err)
		}
	}()

	if cfg.verifyOnly {
		if err := verifyStaticState(setupCtx, c, p, false); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "static configuration verified")
		return err
	}

	if err := c.BecomePrimary(setupCtx); err != nil {
		return fmt.Errorf("become primary: %w", err)
	}
	if err := configureStaticState(setupCtx, c, p); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "static configuration verified"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "controller ready"); err != nil {
		return err
	}
	cancelSetup()
	<-ctx.Done()
	return nil
}

func main() {
	cfg, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "controller:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()
	if err := runController(ctx, os.Stdout, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "controller:", err)
		os.Exit(1)
	}
}
