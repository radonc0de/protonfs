package main

import (
	"testing"
	"time"
)

func TestParseAge(t *testing.T) {
	ok := map[string]time.Duration{
		"": 0, "off": 0, "0": 0,
		"30d": 30 * 24 * time.Hour, "1.5d": 36 * time.Hour,
		"12h": 12 * time.Hour, "90m": 90 * time.Minute,
	}
	for in, want := range ok {
		if got, err := parseAge(in); err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"soon", "-3d", "d", "-1h"} {
		if _, err := parseAge(in); err == nil {
			t.Errorf("parseAge(%q) succeeded, want error", in)
		}
	}
}

func TestParseSize(t *testing.T) {
	ok := map[string]int64{"10G": 10 << 30, "512M": 512 << 20, "1.5GiB": 3 << 29, "100": 100}
	for in, want := range ok {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}
