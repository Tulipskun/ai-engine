package sdk

// SetSubAgentSinks installs the report sink (progress + final handoff
// reports injected into the planner session) and the live trace renderer
// for worker jobs (REQ-019, REQ-021).
func (a *Agent) SetSubAgentSinks(report func(SubAgentEvent), trace func(SubAgentEvent)) {
	if a == nil {
		return
	}
	a.subAgentMu.Lock()
	a.subAgentReportSink = report
	a.subAgentTraceSink = trace
	manager := a.subAgents
	a.subAgentMu.Unlock()
	if manager != nil {
		manager.SetEventSink(report)
		manager.SetTraceSink(trace)
	}
}
