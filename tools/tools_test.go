package tools

import (
	"net"
	"net/url"
	"testing"
)

func TestValidatePublicURLRejectsLocalTargets(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://localhost/",
		"http://service.local/",
	} {
		t.Run(raw, func(t *testing.T) {
			target, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := validatePublicURL(target); err == nil {
				t.Fatalf("validatePublicURL(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestValidatePublicURLAllowsPublicHostnames(t *testing.T) {
	for _, raw := range []string{"https://example.com/", "http://8.8.8.8/"} {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validatePublicURL(target); err != nil {
			t.Errorf("validatePublicURL(%q): %v", raw, err)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip      string
		want    bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"10.1.2.3", false},
		{"100.64.0.1", false},
		{"169.254.169.254", false},
		{"192.0.2.1", false},
		{"::1", false},
		{"fd00::1", false},
		{"2606:4700:4700::1111", true},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			if got := isPublicIP(net.ParseIP(tc.ip)); got != tc.want {
				t.Errorf("isPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}
