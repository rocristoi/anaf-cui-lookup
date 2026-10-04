package main

import "testing"

func TestNormalizeCui(t *testing.T) {
	cases := map[string]string{
		"RO 12345678": "12345678",
		"ro12345678":  "12345678",
		"12345678":    "12345678",
		"1":           "",
		"0000":        "",
		"abc":         "",
		"12345678901": "",
	}
	for in, want := range cases {
		if got := normalizeCui(in); got != want {
			t.Errorf("normalizeCui(%q) = %q, want %q", in, got, want)
		}
	}
}
