package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseCLITargetRequiresExactlyOneProvidedFlag(t *testing.T) {
	tests := []struct {
		name                       string
		zoneProvided, ledsProvided bool
		zone, leds                 string
		wantErr                    bool
	}{
		{name: "zone", zoneProvided: true, zone: "keys"},
		{name: "leds", ledsProvided: true, leds: "1,2"},
		{name: "empty zone", zoneProvided: true, zone: "", wantErr: true},
		{name: "empty leds", ledsProvided: true, leds: "", wantErr: true},
		{name: "both", zoneProvided: true, ledsProvided: true, zone: "keys", leds: "1", wantErr: true},
		{name: "neither", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseCLITarget(test.zoneProvided, test.zone, test.ledsProvided, test.leds)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseCLITarget error = %v, want error=%t", err, test.wantErr)
			}
		})
	}
}

func TestFlagPresenceAndDuplicateSpelling(t *testing.T) {
	if !flagProvided([]string{"-zone", "keys"}, "zone") {
		t.Fatal("single-dash flag was not detected")
	}
	if countFlag([]string{"-zone", "keys", "--zone=backglow"}, "zone") != 2 {
		t.Fatal("mixed duplicate target flags were not counted")
	}
}

func TestParseTTLBoundaries(t *testing.T) {
	for _, value := range []string{"0ms", "999us", "24h1ms", "1.5ms"} {
		if _, err := parseTTL(value); err == nil {
			t.Fatalf("parseTTL(%q) succeeded", value)
		}
	}
	for _, value := range []string{"1ms", "24h"} {
		duration, err := parseTTL(value)
		if err != nil || duration < time.Millisecond {
			t.Fatalf("parseTTL(%q) = %v, %v", value, duration, err)
		}
	}
	if _, err := parseRGB("1,2"); err == nil || !strings.Contains(err.Error(), "three") {
		t.Fatalf("invalid RGB error = %v", err)
	}
}
