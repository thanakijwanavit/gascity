package worker

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// queuedStopProvider returns queued Stop errors, the way the auto composite
// answers a controller that retries an unconfirmed stop every tick.
type queuedStopProvider struct {
	*runtime.Fake
	stopErrs []error
}

func (p *queuedStopProvider) Stop(string) error {
	if len(p.stopErrs) == 0 {
		return nil
	}
	err := p.stopErrs[0]
	p.stopErrs = p.stopErrs[1:]
	return err
}

// g1-c42b7: the refinery kill failed every 12-36 s and each attempt recorded a
// worker.operation event (496 in 3 h). An unconfirmed stop is recorded once
// per backend state; a changed state is recorded again.
func TestRuntimeHandleKillRecordsUnconfirmedStopOncePerState(t *testing.T) {
	recorder := &recordingEventRecorder{}
	first := &runtime.StopUnconfirmedError{
		Name:   "refinery",
		Reason: "acp backend: stop succeeded without liveness confirmation while default backend still reports the session running (launch records: acp=none, default=none)",
	}
	repeat := &runtime.StopUnconfirmedError{Name: first.Name, Reason: first.Reason, Repeat: true}
	changed := &runtime.StopUnconfirmedError{
		Name:   "refinery",
		Reason: "default backend: stop returned but the runtime it started is still running (the configured backend holds no launch record)",
	}
	sp := &queuedStopProvider{Fake: runtime.NewFake(), stopErrs: []error{first, repeat, repeat, changed}}
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "refinery",
		ProviderName: "codex",
		Transport:    "acp",
		Recorder:     recorder,
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	for i := 0; i < 4; i++ {
		if err := handle.Kill(context.Background()); err == nil {
			t.Fatalf("Kill #%d = nil, want the unconfirmed stop", i+1)
		}
	}

	var got []string
	for _, e := range recorder.events {
		var payload operationEventPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("Unmarshal(payload): %v", err)
		}
		if payload.Operation != string(workerOperationKill) {
			continue
		}
		if payload.Result != operationResultFailed {
			t.Fatalf("kill event result = %q, want %q", payload.Result, operationResultFailed)
		}
		got = append(got, payload.Error)
	}
	if want := []string{first.Reason, changed.Reason}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kill events = %q, want one per state %q", got, want)
	}
}
