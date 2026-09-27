package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Tulipskun/ai-engine/sdk"
)

type recorder struct{ frames []Outbound }

func (r *recorder) WriteMessage(_ int, data []byte) error {
	var frame Outbound
	if err := json.Unmarshal(data, &frame); err != nil {
		return err
	}
	r.frames = append(r.frames, frame)
	return nil
}

func (r *recorder) Close() error { return nil }

type allowAllVerifier struct{}

func (allowAllVerifier) VerifyToken(context.Context, string) error { return nil }

func newDisplayTransport() *Transport {
	return New(Config{Tokens: &stubTokens{}, Verifier: allowAllVerifier{}})
}

func capture(t *testing.T, tr *Transport, run func()) []Outbound {
	t.Helper()
	rec := &recorder{}
	tr.subscribe("s1", rec)
	run()
	return rec.frames
}

func textTrace(stage sdk.TraceStage, text string) sdk.Output {
	return sdk.Output{
		Source: SourceName, SessionID: "s1",
		Trace: &sdk.TraceEvent{Stage: stage, Text: text},
	}
}

func responseTrace(stage sdk.TraceStage, text string) sdk.Output {
	return sdk.Output{
		Source: SourceName, SessionID: "s1",
		Trace: &sdk.TraceEvent{Stage: stage, Response: &sdk.Response{
			Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: text}},
			Usage:   sdk.Usage{InputTokens: 12, OutputTokens: 34},
		}},
	}
}

// A streamed answer arrives as deltas; the closing frame carries the whole
// answer with what the provider charged, because that is the only frame with
// token counts, and then the turn closes.
func TestStreamedTextBecomesDeltasAndTheTerminalCarriesTheUsage(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), textTrace(sdk.TraceResponseContent, "สวัส"))
		_ = tr.Display(context.Background(), textTrace(sdk.TraceResponseContent, "ดี"))
		_ = tr.Display(context.Background(), responseTrace(sdk.TraceResponse, "สวัสดี"))
	})
	if len(frames) != 4 {
		t.Fatalf("frames = %+v, want two deltas, the counted answer and a done", frames)
	}
	if frames[0].Kind != FrameDelta || frames[0].Text != "สวัส" || frames[0].Role != "model" {
		t.Fatalf("first frame = %+v, want a model delta", frames[0])
	}
	if frames[1].Kind != FrameDelta || frames[1].Text != "ดี" {
		t.Fatalf("second frame = %+v, want the next delta", frames[1])
	}
	if frames[2].Kind != FrameMessage || frames[2].Text != "สวัสดี" ||
		frames[2].InputTokens != 12 || frames[2].OutputTokens != 34 {
		t.Fatalf("closing answer = %+v, want the whole text and its usage", frames[2])
	}
	if frames[3].Kind != FrameDone {
		t.Fatalf("last frame = %+v, want done after the counted answer", frames[3])
	}
}

func TestNonStreamedAnswerIsOneMessageThenDone(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), responseTrace(sdk.TraceResponseContent, "ตอบเลย"))
		_ = tr.Display(context.Background(), responseTrace(sdk.TraceResponse, "ตอบเลย"))
	})
	if len(frames) != 2 || frames[0].Kind != FrameMessage || frames[1].Kind != FrameDone {
		t.Fatalf("frames = %+v, want message then done", frames)
	}
	if frames[0].Text != "ตอบเลย" || frames[0].InputTokens != 12 || frames[0].OutputTokens != 34 {
		t.Fatalf("message = %+v, want the text and usage", frames[0])
	}
}

func TestToolStagesCarryTheCallAndTheResult(t *testing.T) {
	tr := newDisplayTransport()
	call := &sdk.ToolCall{ID: "call-1", Name: "read", Arguments: `{"path":"index.md"}`}
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1",
			Trace: &sdk.TraceEvent{Stage: sdk.TraceToolCall, ToolCall: call}})
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1",
			Trace: &sdk.TraceEvent{Stage: sdk.TraceToolResult, ToolCall: call,
				ToolResult: &sdk.ToolResult{ID: "call-1", Content: "พบ 3 ไฟล์"}}})
	})
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want two tool frames", frames)
	}
	if frames[0].Stage != string(sdk.TraceToolCall) || frames[0].ToolCall == nil || frames[0].ToolCall.Name != "read" {
		t.Fatalf("tool call frame = %+v", frames[0])
	}
	if frames[1].Stage != string(sdk.TraceToolResult) || frames[1].ToolResult == nil ||
		frames[1].ToolResult.Text != "พบ 3 ไฟล์" {
		t.Fatalf("tool result frame = %+v", frames[1])
	}
}

// A streamed sub agent answers with a delta, then the authoritative message that
// carries what the turn cost, then the frame that closes it. The final message is
// what puts real token counts on the phone's footer instead of an estimate.
func TestSubagentTextIsAttributedAndDeltasAreMarkedSub(t *testing.T) {
	tr := newDisplayTransport()
	meta := map[string]string{"trace_actor": "subagent", "trace_job_id": "sa-42"}
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1", Metadata: meta,
			Trace: &sdk.TraceEvent{Stage: sdk.TraceResponseContent, Text: "กำลังอ่านไฟล์"}})
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1", Metadata: meta,
			Trace: &sdk.TraceEvent{Stage: sdk.TraceResponse, Response: &sdk.Response{
				Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "กำลังอ่านไฟล์"}},
				Usage:   sdk.Usage{InputTokens: 120, OutputTokens: 9}}}})
	})
	if len(frames) != 4 {
		t.Fatalf("frames = %+v, want a sub delta, the counted answer, the close and the job's end", frames)
	}
	if frames[0].Agent != "sub" || frames[0].JobID != "sa-42" || frames[0].Kind != FrameDelta {
		t.Fatalf("sub frame = %+v", frames[0])
	}
	if frames[1].Kind != FrameMessage || frames[1].JobID != "sa-42" ||
		frames[1].InputTokens != 120 || frames[1].OutputTokens != 9 {
		t.Fatalf("final message = %+v, want the sub agent's token counts", frames[1])
	}
	if frames[2].Kind != FrameDone || frames[2].JobID != "sa-42" {
		t.Fatalf("closing frame = %+v", frames[2])
	}
	// The last frame is what settles the phone's row for this worker.
	if frames[3].Kind != FrameDone || frames[3].JobID != "sa-42" || frames[3].Stage != "subagent_completed" {
		t.Fatalf("job end frame = %+v, want the worker's terminal state", frames[3])
	}
}

func TestReasoningBecomesAThinkingLineNotAMessage(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), textTrace(sdk.TraceResponseText, "กำลังคิด…"))
	})
	if len(frames) != 1 || frames[0].Kind != FrameTrace || frames[0].Stage != string(sdk.TraceResponseText) {
		t.Fatalf("frames = %+v, want one thinking trace", frames)
	}
}

func TestReasoningReportsRecordedTimeWithoutItsText(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1",
			Trace: &sdk.TraceEvent{Stage: sdk.TraceResponseText, Text: "secret chain of thought",
				RequestStartedMs: 1_700_000_000_000, AtMs: 1_700_000_001_500}})
	})
	if len(frames) != 1 {
		t.Fatalf("frames = %+v, want one thinking trace", frames)
	}
	if frames[0].Text != "thinking" || frames[0].ReasoningMs != 1500 {
		t.Fatalf("frame = %+v, want fixed thinking text and 1500ms", frames[0])
	}
}

func TestToolResultReportsRecordedDuration(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1",
			Trace: &sdk.TraceEvent{Stage: sdk.TraceToolResult,
				ToolCall:         &sdk.ToolCall{ID: "c1", Name: "bash", Arguments: "{}"},
				ToolResult:       &sdk.ToolResult{ID: "c1", Content: "ok"},
				RequestStartedMs: 1_700_000_000_000, AtMs: 1_700_000_002_250}})
	})
	if len(frames) != 1 || frames[0].Kind != FrameTrace || frames[0].Stage != string(sdk.TraceToolResult) {
		t.Fatalf("frames = %+v, want one tool-result trace", frames)
	}
	if frames[0].ToolDurationMs != 2250 {
		t.Fatalf("tool duration = %dms, want 2250ms", frames[0].ToolDurationMs)
	}
}

func TestTraceErrorBecomesAnErrorFrame(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1",
			Trace: &sdk.TraceEvent{Stage: sdk.TraceError, Message: "โควตาไม่พอ"}})
	})
	if len(frames) != 1 || frames[0].Kind != FrameError || frames[0].Text != "โควตาไม่พอ" {
		t.Fatalf("frames = %+v, want the provider error text", frames)
	}
}

func TestFinalTextOnlyCountsTerminalEvents(t *testing.T) {
	if got := FinalText(textTrace(sdk.TraceResponseContent, "ทีละคัด")); got != "" {
		t.Fatalf("FinalText(delta) = %q, want empty so D1 does not grow a turn per chunk", got)
	}
	if got := FinalText(responseTrace(sdk.TraceResponse, "คำตอบเต็ม")); got != "คำตอบเต็ม" {
		t.Fatalf("FinalText(terminal) = %q, want the full answer", got)
	}
	if got := FinalText(sdk.Output{Source: SourceName, SessionID: "s1",
		Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "ไม่มี trace"}}}); got != "ไม่มี trace" {
		t.Fatalf("FinalText(untraced) = %q", got)
	}
}

func TestOtherSourcesAndEmptyTextAreIgnored(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: "discord", SessionID: "s1",
			Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "ไม่ใช่ของเรา"}}})
		_ = tr.Display(context.Background(), textTrace(sdk.TraceResponseContent, ""))
	})
	if len(frames) != 0 {
		t.Fatalf("frames = %+v, want none", frames)
	}
}

func TestCancelStageTellsThePhoneWhetherItStoppedAnything(t *testing.T) {
	if got := cancelStage(true); got != "cancelled" {
		t.Fatalf("cancelStage(true) = %q", got)
	}
	if got := cancelStage(false); got != "already_done" {
		t.Fatalf("cancelStage(false) = %q", got)
	}
}

// A turn the phone stopped must not read as a provider failure.
func TestCancelledTraceBecomesTheStopFrame(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1",
			Trace: &sdk.TraceEvent{
				Stage: sdk.TraceError,
				Err:   fmt.Errorf("chat/completions: %w", context.Canceled),
			}})
	})
	if len(frames) != 1 || frames[0].Kind != FrameDone || frames[0].Stage != "cancelled" {
		t.Fatalf("frames = %+v, want a single done frame marked cancelled", frames)
	}
}

// A sub agent that was stopped ends with a cancelled provider error. The phone
// must read that as one worker stopping, not as the whole turn stopping.
func TestStoppedSubAgentEndsOnlyItsOwnRow(t *testing.T) {
	tr := newDisplayTransport()
	meta := map[string]string{"trace_actor": "subagent", "trace_job_id": "sa-9"}
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{Source: SourceName, SessionID: "s1", Metadata: meta,
			Trace: &sdk.TraceEvent{Stage: sdk.TraceError, Err: fmt.Errorf("chat/completions: %w", context.Canceled)}})
	})
	if len(frames) != 1 {
		t.Fatalf("frames = %+v, want one frame", frames)
	}
	if frames[0].Kind != FrameDone || frames[0].Stage != "subagent_stopped" || frames[0].JobID != "sa-9" {
		t.Fatalf("frame = %+v, want the stopped worker named by its job", frames[0])
	}
}

// A worker the phone stopped has no terminal trace, so its final report is what
// settles the row.
func TestSubAgentTerminalNamesTheStoppedJob(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		tr.SubAgentTerminal("s1", "sa-3", "stopped")
		tr.SubAgentTerminal("s1", "", "stopped")
		tr.SubAgentTerminal("s1", "sa-4", "weird-status")
	})
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want one frame per named job", frames)
	}
	if frames[0].Kind != FrameDone || frames[0].JobID != "sa-3" || frames[0].Stage != "subagent_stopped" {
		t.Fatalf("frame = %+v, want the stopped worker named", frames[0])
	}
	if frames[1].JobID != "sa-4" || frames[1].Stage != "subagent_completed" {
		t.Fatalf("frame = %+v, want an unknown status read as completed", frames[1])
	}
}

// The footer of one answer says which model produced it and how long it took,
// measured from the timestamps the trace recorded rather than at draw time
// (AX-095).
func TestAnswerFrameCarriesItsFooter(t *testing.T) {
	tr := newDisplayTransport()
	frames := capture(t, tr, func() {
		_ = tr.Display(context.Background(), sdk.Output{
			SessionID: "s1",
			Trace: &sdk.TraceEvent{
				Stage: sdk.TraceResponse,
				Response: &sdk.Response{
					Model:   "nemotron-3-ultra-free",
					Content: []sdk.ContentPart{{Type: sdk.ContentText, Text: "pong"}},
					Usage:   sdk.Usage{InputTokens: 2269, OutputTokens: 51, CacheReadTokens: 1800, CacheWriteTokens: 12},
				},
				RequestStartedMs: 1_700_000_000_000,
				AtMs:             1_700_000_004_000,
			},
		})
	})
	var answer Outbound
	for _, f := range frames {
		if f.Kind == FrameMessage {
			answer = f
		}
	}
	if answer.Model != "nemotron-3-ultra-free" {
		t.Errorf("frame model = %q, want nemotron-3-ultra-free", answer.Model)
	}
	if answer.InputTokens != 2269 || answer.OutputTokens != 51 {
		t.Errorf("counts = %d/%d, want 2269/51", answer.InputTokens, answer.OutputTokens)
	}
	if answer.CacheReadTokens != 1800 || answer.CacheWriteTokens != 12 {
		t.Errorf("cache = %d/%d, want 1800/12", answer.CacheReadTokens, answer.CacheWriteTokens)
	}
	if answer.DurationMs != 4000 {
		t.Errorf("duration = %dms, want 4000ms", answer.DurationMs)
	}
}

// A turn that fell back to the trace's own elapsed time still gets a duration.
func TestAnswerFrameDurationFallsBackToTheTracesElapsed(t *testing.T) {
	if got := turnDurationMs(&sdk.TraceEvent{Elapsed: 1500 * 1000 * 1000}); got != 1500 {
		t.Fatalf("duration = %d, want 1500", got)
	}
	if got := turnDurationMs(&sdk.TraceEvent{RequestStartedMs: 500, AtMs: 400}); got != 0 {
		t.Fatalf("a negative window became %d", got)
	}
	if got := turnDurationMs(nil); got != 0 {
		t.Fatalf("no trace became %d", got)
	}
}
