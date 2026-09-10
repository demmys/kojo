package agent

import (
	"context"
	"testing"
	"time"
)

func TestProcessOneShotEventsDeliversTerminalAfterCancelUnderBackpressure(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	outCh := make(chan ChatEvent, 1)
	backendCh := make(chan ChatEvent, 1)

	outCh <- ChatEvent{Type: "status", Status: "buffer-full"}
	backendCh <- ChatEvent{Type: "done", ErrorMessage: ErrMsgCancelled}
	close(backendCh)
	cancel()

	done := make(chan struct{})
	go func() {
		m.processOneShotEvents(ctx, "test-agent", backendCh, outCh, true)
		close(done)
	}()

	// Keep the adapter busy longer than the old fixed 250 ms grace. Slack API
	// finalization can legitimately take longer; the authoritative terminal
	// must remain pending until this live consumer drains.
	time.Sleep(300 * time.Millisecond)
	<-outCh
	select {
	case got := <-outCh:
		if got.Type != "done" || got.ErrorMessage != ErrMsgCancelled {
			t.Fatalf("terminal event = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event was dropped after cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processOneShotEvents did not return")
	}
}

func TestProcessOneShotEventsRejectsAttachmentsWhileDrainingCancelledTurn(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	outCh := make(chan ChatEvent, 1)
	backendCh := make(chan ChatEvent, 2)
	outCh <- ChatEvent{Type: "status", Status: "buffer-full"}
	attachment := ChatEvent{Type: "attachment", attachmentClaim: newAttachmentOwnership()}
	backendCh <- attachment
	backendCh <- ChatEvent{Type: "done", ErrorMessage: ErrMsgCancelled}
	close(backendCh)
	cancel()

	done := make(chan struct{})
	go func() {
		m.processOneShotEvents(ctx, "test-agent", backendCh, outCh, true)
		close(done)
	}()
	select {
	case <-attachment.attachmentClaim.done:
	case <-time.After(time.Second):
		t.Fatal("cancelled attachment was not rejected")
	}
	<-outCh // release the deliberately full adapter buffer
	select {
	case terminal := <-outCh:
		if terminal.Type != "done" || terminal.ErrorMessage != ErrMsgCancelled {
			t.Fatalf("terminal event = %#v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal after cancelled attachment was not preserved")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processOneShotEvents did not return")
	}
	if attachment.BeginAttachmentOwnership() {
		t.Fatal("cancelled drain left attachment ownership claimable")
	}
}
