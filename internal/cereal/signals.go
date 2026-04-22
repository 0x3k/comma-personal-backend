package cereal

import (
	"fmt"
	"io"
	"time"

	"comma-personal-backend/internal/cereal/schema"
)

// DrivingSignals is a column-oriented, time-aligned view of a log. Each slice
// is the same length as Times; entry i of every slice describes the same
// moment. Missing values are encoded as zero for primitive fields and as ""
// for AlertText.
//
// The signals are sampled at whatever cadence the events arrive on the log:
//   - vEgo, steeringAngleDeg, brakePressed, gasPressed come from carState (100 Hz in rlog, ~5 Hz in qlog)
//   - engaged, alertText come from selfdriveState / controlsState (100 Hz / ~5 Hz)
//   - deviceState thermalStatus isn't returned here (it's a separate signal
//     stream); callers that need it can use the lower-level Parser.
type DrivingSignals struct {
	Times            []time.Time
	VEgo             []float64
	SteeringAngleDeg []float64
	BrakePressed     []bool
	GasPressed       []bool
	Engaged          []bool
	AlertText        []string
}

// SignalExtractor pulls the downstream-relevant signals from a cereal log.
// It is a thin convenience wrapper around Parser; construct one with the
// zero value (&SignalExtractor{}) for default parsing options, or set fields
// on the embedded Parser to tune decode limits.
type SignalExtractor struct {
	Parser Parser
}

// ExtractDriving streams the log in r and returns a DrivingSignals value.
// Every event in the log contributes one row to the output -- rows are keyed
// on the event's logMonoTime (converted to a time.Time via time.Unix). Each
// row is filled only for the signal carried by that event; all other columns
// of that row carry the zero value so the slices stay aligned.
//
// Both uncompressed and bz2-compressed streams are accepted (the underlying
// Parser auto-detects).
func (e *SignalExtractor) ExtractDriving(r io.Reader) (*DrivingSignals, error) {
	if e == nil {
		e = &SignalExtractor{}
	}
	out := &DrivingSignals{}

	err := e.Parser.Parse(r, func(evt schema.Event) error {
		which := evt.Which()

		// Only rows carrying a signal we track contribute to the output.
		// Anything else (camera frames, managerState, ...) is ignored so
		// the slices stay compact.
		var (
			contributes      bool
			vEgo             float64
			steeringAngleDeg float64
			brakePressed     bool
			gasPressed       bool
			engaged          bool
			engagedSet       bool
			alert            string
			alertSet         bool
		)

		switch which {
		case schema.Event_Which_carState:
			cs, err := evt.CarState()
			if err != nil {
				return fmt.Errorf("cereal: read carState: %w", err)
			}
			vEgo = float64(cs.VEgo())
			steeringAngleDeg = float64(cs.SteeringAngleDeg())
			brakePressed = cs.BrakePressed()
			gasPressed = cs.GasPressed()
			contributes = true

		case schema.Event_Which_selfdriveState:
			ss, err := evt.SelfdriveState()
			if err != nil {
				return fmt.Errorf("cereal: read selfdriveState: %w", err)
			}
			engaged = ss.Enabled()
			engagedSet = true
			text, err := ss.AlertText1()
			if err != nil {
				return fmt.Errorf("cereal: read selfdriveState.alertText1: %w", err)
			}
			alert = text
			alertSet = true
			contributes = true

		case schema.Event_Which_controlsState:
			// Older logs pre-date the SelfdriveState split; fall back to
			// the deprecated ControlsState fields so we still surface a
			// useful engaged/alert signal for historical data.
			cs, err := evt.ControlsState()
			if err != nil {
				return fmt.Errorf("cereal: read controlsState: %w", err)
			}
			engaged = cs.EnabledDEPRECATED()
			engagedSet = true
			text, err := cs.AlertText1DEPRECATED()
			if err != nil {
				return fmt.Errorf("cereal: read controlsState.alertText1DEPRECATED: %w", err)
			}
			alert = text
			alertSet = true
			contributes = true
		}

		if !contributes {
			return nil
		}

		// logMonoTime is nanoseconds since the device's monotonic clock
		// origin (not wall-clock). That's fine for downstream consumers
		// that treat Times as a per-log relative timeline.
		mono := evt.LogMonoTime()
		t := time.Unix(0, int64(mono)).UTC()

		out.Times = append(out.Times, t)
		out.VEgo = append(out.VEgo, vEgo)
		out.SteeringAngleDeg = append(out.SteeringAngleDeg, steeringAngleDeg)
		out.BrakePressed = append(out.BrakePressed, brakePressed)
		out.GasPressed = append(out.GasPressed, gasPressed)
		// Fill engaged/alert only if the event carried them; otherwise
		// leave the zero value so CarState rows don't clobber a previously
		// observed engagement state.
		if engagedSet {
			out.Engaged = append(out.Engaged, engaged)
		} else {
			out.Engaged = append(out.Engaged, false)
		}
		if alertSet {
			out.AlertText = append(out.AlertText, alert)
		} else {
			out.AlertText = append(out.AlertText, "")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
