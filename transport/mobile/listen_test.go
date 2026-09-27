package mobile

import (
	"context"
	"net"
	"strings"
	"testing"
)

// A daemon that cannot listen must say so, not publish a tunnel that 502s.
func TestStartHTTPFailsWhenThePortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tr := New(Config{Verifier: allowVerifier{}})
	if _, err := tr.StartHTTP(context.Background(), ln.Addr().String()); err == nil {
		t.Fatal("a taken port must be an error, not a tunnel that answers 502")
	} else if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("error should name the listen failure, got %v", err)
	}
}
