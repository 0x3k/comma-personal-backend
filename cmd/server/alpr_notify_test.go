package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/alpr/notify"
	"comma-personal-backend/internal/config"
)

// TestBuildALPRNotifyDispatcher_NilCfg confirms the dispatcher
// constructor refuses to build when given a nil config (the boot path
// always passes a real *ALPRConfig, but the nil-guard keeps a future
// mis-wiring from segfaulting).
func TestBuildALPRNotifyDispatcher_NilCfg(t *testing.T) {
	if got := buildALPRNotifyDispatcher(nil, nil, nil); got != nil {
		t.Fatalf("expected nil dispatcher for nil cfg, got %+v", got)
	}
}

// TestBuildALPRNotifyDispatcher_NoSenders confirms the dispatcher is
// non-nil with zero senders when no notify env vars are set. This is
// the "no-config no-op" acceptance criterion: the AlertCreated
// subscriber still drains the channel cleanly even though no transport
// is configured.
func TestBuildALPRNotifyDispatcher_NoSenders(t *testing.T) {
	cfg := &config.ALPRConfig{
		NotifyMinSeverity: 4,
		NotifyDedupHours:  12,
		NotifySMTPPort:    587,
		NotifySMTPTLS:     "starttls",
	}
	d := buildALPRNotifyDispatcher(cfg, nil, nil)
	if d == nil {
		t.Fatal("expected non-nil dispatcher even without senders")
	}
	if d.SenderCount() != 0 {
		t.Errorf("SenderCount = %d, want 0", d.SenderCount())
	}
}

// TestBuildALPRNotifyDispatcher_EmailSender confirms the email sender
// is wired when the SMTP knobs are sufficient to construct it.
func TestBuildALPRNotifyDispatcher_EmailSender(t *testing.T) {
	cfg := &config.ALPRConfig{
		NotifyMinSeverity: 4,
		NotifyDedupHours:  12,
		NotifySMTPHost:    "smtp.example.com",
		NotifySMTPPort:    587,
		NotifySMTPUser:    "alerts@example.com",
		NotifySMTPFrom:    "alerts@example.com",
		NotifySMTPTLS:     "starttls",
		NotifyEmailTo:     "user@example.com",
	}
	d := buildALPRNotifyDispatcher(cfg, nil, nil)
	if d.SenderCount() != 1 {
		t.Fatalf("SenderCount = %d, want 1 (email)", d.SenderCount())
	}
	names := d.SenderNames()
	if names[0] != "email" {
		t.Errorf("first sender = %q, want email", names[0])
	}
}

// TestBuildALPRNotifyDispatcher_WebhookSender confirms the webhook
// sender is wired when only ALPR_NOTIFY_WEBHOOK_URL is set.
func TestBuildALPRNotifyDispatcher_WebhookSender(t *testing.T) {
	cfg := &config.ALPRConfig{
		NotifyMinSeverity: 4,
		NotifyDedupHours:  12,
		NotifySMTPPort:    587,
		NotifySMTPTLS:     "starttls",
		NotifyWebhookURL:  "https://hooks.example.com/alpr",
	}
	d := buildALPRNotifyDispatcher(cfg, nil, nil)
	if d.SenderCount() != 1 {
		t.Fatalf("SenderCount = %d, want 1 (webhook)", d.SenderCount())
	}
	names := d.SenderNames()
	if names[0] != "webhook" {
		t.Errorf("first sender = %q, want webhook", names[0])
	}
}

// TestRunALPRNotifySubscriber_Drains confirms the subscriber drains
// the channel even when the dispatcher is nil. This is the path a
// deployment without notify env vars takes: the heuristic still emits
// AlertCreated, and somebody has to drink the channel.
func TestRunALPRNotifySubscriber_Drains(t *testing.T) {
	alerts := make(chan heuristic.AlertCreated, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runALPRNotifySubscriber(ctx, nil, alerts)
	}()
	for i := 0; i < 4; i++ {
		alerts <- heuristic.AlertCreated{Severity: 5, Route: "r"}
	}
	// Channel writes should have all returned; the subscriber drained
	// them. Cancelling the context unblocks the Run loop's exit.
	cancel()
	close(alerts)
	wg.Wait()
}

// TestRunALPRNotifySubscriber_DispatchesAlert confirms a real
// AlertCreated event reaches the dispatcher and the configured sender.
func TestRunALPRNotifySubscriber_DispatchesAlert(t *testing.T) {
	called := make(chan struct{}, 1)
	sender := senderFunc{
		name: "test",
		send: func(_ context.Context, _ notify.AlertPayload) error {
			called <- struct{}{}
			return nil
		},
	}
	d := notify.New(nil, []notify.Sender{sender}, notify.PayloadEnricherFunc(
		func(_ context.Context, p notify.AlertPayload) (notify.AlertPayload, error) {
			p.Plate = "ABC-123"
			return p, nil
		},
	), notify.Config{MinSeverity: 4, DedupHours: 0})

	alerts := make(chan heuristic.AlertCreated, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runALPRNotifySubscriber(ctx, d, alerts)

	plateHash := make([]byte, 32)
	alerts <- heuristic.AlertCreated{
		PlateHash: plateHash,
		Severity:  5,
		Route:     "r",
		DongleID:  "d",
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("sender was not called within 2s")
	}
}

// senderFunc is a tiny adapter so tests can build a Sender from a
// closure without a struct boilerplate.
type senderFunc struct {
	name string
	send func(ctx context.Context, alert notify.AlertPayload) error
}

func (s senderFunc) Name() string { return s.name }
func (s senderFunc) Send(ctx context.Context, alert notify.AlertPayload) error {
	return s.send(ctx, alert)
}
