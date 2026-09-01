package main

import "testing"

func TestParseOptionsDefaults(t *testing.T) {
	cfg, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.address != "127.0.0.1:50052" || cfg.deviceID != 2 || cfg.electionID != 2 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.verifyOnly {
		t.Fatal("verify-only enabled by default")
	}
}

func TestParseOptionsRejectsInvalidIdentifiers(t *testing.T) {
	for _, args := range [][]string{{"--device-id", "0"}, {"--election-id", "0"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
}

func TestParseOptionsRejectsPositionalArguments(t *testing.T) {
	if _, err := parseOptions([]string{"unexpected"}); err == nil {
		t.Fatal("positional argument was accepted")
	}
}

func TestParseOptionsExpectedMAC(t *testing.T) {
	cfg, err := parseOptions([]string{
		"--expect-mac", "00:00:00:00:00:01",
		"--expect-port", "3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.expectedMAC == nil || cfg.expectedMAC.mac.String() != "00:00:00:00:00:01" ||
		cfg.expectedMAC.port != 3 {
		t.Fatalf("unexpected learned-state expectation: %+v", cfg.expectedMAC)
	}
	if cfg.electionID != 1 {
		t.Fatalf("readback election ID = %d, want 1", cfg.electionID)
	}
}

func TestParseOptionsReadOnlyElectionID(t *testing.T) {
	verify, err := parseOptions([]string{"--verify-only"})
	if err != nil {
		t.Fatal(err)
	}
	if verify.electionID != 1 {
		t.Fatalf("verify-only election ID = %d, want 1", verify.electionID)
	}
	explicit, err := parseOptions([]string{"--verify-only", "--election-id", "7"})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.electionID != 7 {
		t.Fatalf("explicit election ID = %d, want 7", explicit.electionID)
	}
}

func TestParseOptionsRejectsInvalidExpectedMAC(t *testing.T) {
	tests := [][]string{
		{"--expect-mac", "00:00:00:00:00:01"},
		{"--expect-port", "1"},
		{"--expect-mac", "01:00:00:00:00:01", "--expect-port", "1"},
		{"--expect-mac", "00:00:00:00:00:01", "--expect-port", "5"},
		{"--verify-only", "--expect-mac", "00:00:00:00:00:01", "--expect-port", "1"},
	}
	for _, args := range tests {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("invalid expected state accepted: %v", args)
		}
	}
}
