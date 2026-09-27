package mobile

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A cancel frame that names a sub agent must stop that worker only and answer
// for the job, while the turn keeps running.
func TestCancelWithJobIDStopsOneSubAgent(t *testing.T) {
	tokens := &recordingTokens{}
	transport := New(Config{Tokens: tokens, Verifier: &stubVerifier{}})
	asked := ""
	transport.SetCancel(func(string) bool {
		t.Fatal("a cancel that names a job must not stop the whole turn")
		return false
	})
	transport.SetCancelSubAgent(func(sessionID, jobID string) error {
		asked = sessionID + "/" + jobID
		return nil
	})
	server := httptest.NewServer(http.HandlerFunc(transport.serveWS))
	defer server.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer good-token")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(Inbound{Type: FrameHello, SessionID: "s1", Source: SourceName}); err != nil {
		t.Fatal(err)
	}
	ack := readFrame(t, conn)
	if ack.Kind != FrameAck {
		t.Fatalf("first frame = %+v, want the handshake ack", ack)
	}
	if err := conn.WriteJSON(Inbound{Type: FrameCancel, SessionID: "s1", JobID: "sa-7"}); err != nil {
		t.Fatal(err)
	}
	reply := readFrame(t, conn)
	if reply.Kind != FrameDone || reply.Stage != "subagent_stopping" || reply.JobID != "sa-7" {
		t.Fatalf("reply = %+v, want a done frame for sa-7 saying the stop was accepted", reply)
	}
	if asked != "s1/sa-7" {
		t.Fatalf("daemon asked to stop %q, want s1/sa-7", asked)
	}
}

// A job the phone cannot find must be named back, not swallowed.
func TestCancelWithUnknownJobIsReported(t *testing.T) {
	if got := subAgentStopStage(errors.New("sdk: sub-agent job not found: sa-9")); got != "subagent_not_found" {
		t.Fatalf("stage = %q, want subagent_not_found", got)
	}
	if got := subAgentStopStage(errors.New("provider is on fire")); got != "subagent_stop_failed" {
		t.Fatalf("stage = %q, want subagent_stop_failed", got)
	}
	if got := subAgentStopStage(nil); got != "subagent_stopping" {
		t.Fatalf("stage = %q, want subagent_stopping", got)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) Outbound {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var frame Outbound
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return frame
}
