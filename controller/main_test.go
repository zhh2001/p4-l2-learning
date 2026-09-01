package main

import "testing"

func TestParseOptionsDefaults(t *testing.T) {
	cfg, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.address != "127.0.0.1:50052" || cfg.deviceID != 2 || cfg.electionID != 1 {
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
