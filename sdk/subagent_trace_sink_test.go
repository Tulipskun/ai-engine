package sdk

import "testing"

func TestSubAgentTraceSinkDeliversWorkerTraces(t *testing.T) {
	manager := newSubAgentManager(nil, SubAgentConfig{})
	traceCh := make(chan SubAgentEvent, 2)
	manager.SetTraceSink(func(event SubAgentEvent) { traceCh <- event })

	manager.emitTrace(&subAgentJob{id: "sa-test", parent: NewSession(SessionConfig{ID: "parent"}, nil)}, TraceEvent{Stage: TraceToolCall, RequestStartedMs: 1000, ProviderAcceptedMs: 1500, AtMs: 2000})
	select {
	case event := <-traceCh:
		if event.Trace == nil || event.Trace.Stage != TraceToolCall {
			t.Fatalf("unexpected trace event: %+v", event)
		}
		if event.Trace.RequestStartedMs != 1000 || event.Trace.ProviderAcceptedMs != 1500 || event.Trace.AtMs != 2000 {
			t.Fatalf("trace lost timestamp fields: %+v", event.Trace)
		}
	default:
		t.Fatal("trace sink did not receive live event")
	}
	select {
	case event := <-traceCh:
		t.Fatalf("sink received unexpected extra event: %+v", event)
	default:
	}
}
