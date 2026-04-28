package notify

import "context"

// Sender is the small contract the dispatcher uses to talk to a
// transport. Implementations must be safe for concurrent use because
// the dispatcher fans out to every sender in parallel for a single
// alert. Each Send call is given its own ctx with a 10s timeout (set
// by the dispatcher); honouring ctx.Done() is a soft requirement --
// the dispatcher uses ctx.Err() to surface a timeout in the result
// log.
type Sender interface {
	// Name is a stable identifier the dispatcher logs and the test
	// endpoint reports back. Currently "email" or "webhook"; new
	// senders must add a unique name.
	Name() string

	// Send transmits a single alert. Errors are returned to the
	// dispatcher; the dispatcher logs at warn and (importantly) does
	// NOT propagate to upstream callers. A nil error means the
	// outbound transport accepted the alert; what happens after that
	// (e.g. a webhook receiver returning 500) is captured in the
	// returned error.
	Send(ctx context.Context, alert AlertPayload) error
}
