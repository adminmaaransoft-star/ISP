package billing

import (
	"context"
	"fmt"
	"time"
)

// DunningState represents a subscriber's collection lifecycle stage.
// FR: FR-BIL-004 | DDS §5.4
type DunningState string

const (
	DunningActive        DunningState = "active"
	DunningRemind7d      DunningState = "remind_7d"
	DunningRemind3d      DunningState = "remind_3d"
	DunningRemind1d      DunningState = "remind_1d"
	DunningGracePeriod   DunningState = "grace_period"
	DunningSoftSuspended DunningState = "soft_suspended"
	DunningHardSuspended DunningState = "hard_suspended"
)

// DunningTransition describes a valid state machine edge.
type DunningTransition struct {
	From      DunningState
	To        DunningState
	Condition string
}

// validTransitions lists all allowed dunning state machine edges.
var validTransitions = []DunningTransition{
	{DunningActive, DunningRemind7d, "plan_expiry - now <= 7 days"},
	{DunningRemind7d, DunningRemind3d, "plan_expiry - now <= 3 days"},
	{DunningRemind3d, DunningRemind1d, "plan_expiry - now <= 1 day"},
	{DunningRemind1d, DunningGracePeriod, "plan_expiry <= now"},
	{DunningGracePeriod, DunningSoftSuspended, "grace_period expired (3 days)"},
	{DunningSoftSuspended, DunningHardSuspended, "soft_suspension period expired"},
	// Restore edges (triggered by payment)
	{DunningGracePeriod, DunningActive, "payment received"},
	{DunningSoftSuspended, DunningActive, "payment received"},
	{DunningHardSuspended, DunningActive, "payment received"},
}

// DunningQuerier is the DB interface needed by the dunning state machine.
type DunningQuerier interface {
	GetSubscriberDunningState(ctx context.Context, subscriberID int) (DunningState, time.Time, error)
	SetSubscriberDunningState(ctx context.Context, subscriberID int, state DunningState, status string) error
}

// TransitionDunning advances a subscriber's dunning state if the given target
// transition is permitted by the state machine.
//
// FR: FR-BIL-004
func TransitionDunning(ctx context.Context, db DunningQuerier, subscriberID int, targetState DunningState) error {
	currentState, _, err := db.GetSubscriberDunningState(ctx, subscriberID)
	if err != nil {
		return fmt.Errorf("dunning: get state: %w", err)
	}

	for _, t := range validTransitions {
		if t.From == currentState && t.To == targetState {
			// Map dunning state to subscriber.status for RADIUS enforcement
			subscriberStatus := dunningToSubscriberStatus(targetState)
			if err := db.SetSubscriberDunningState(ctx, subscriberID, targetState, subscriberStatus); err != nil {
				return fmt.Errorf("dunning: set state %s → %s: %w", currentState, targetState, err)
			}
			return nil
		}
	}
	return fmt.Errorf("dunning: invalid transition %s → %s", currentState, targetState)
}

// dunningToSubscriberStatus maps dunning states to the subscribers.status column.
func dunningToSubscriberStatus(d DunningState) string {
	switch d {
	case DunningActive, DunningRemind7d, DunningRemind3d, DunningRemind1d:
		return "active"
	case DunningGracePeriod:
		return "grace_period"
	case DunningSoftSuspended:
		return "soft_suspended"
	case DunningHardSuspended:
		return "hard_suspended"
	default:
		return "active"
	}
}
