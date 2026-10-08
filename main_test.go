package main

import "testing"

func TestParseStarted(t *testing.T) {
	cases := map[string]int64{
		"ai-engine/1.1 started=1791457000": 1791457000,
		"ai-engine/1.0":                    0,
		"":                                 0,
		"ai-engine/1.1 started=abc":        0,
	}
	for in, want := range cases {
		if got := parseStarted(in); got != want {
			t.Errorf("parseStarted(%q) = %d, want %d", in, got, want)
		}
	}
}
