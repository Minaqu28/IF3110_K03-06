package domain

import "testing"

func TestMigrationActionSemantics(t *testing.T) {
	cases := []struct {
		action         MigrationAction
		valid          bool
		direction      Direction
		requiresTarget bool
	}{
		{MigrationActionApply, true, DirectionUp, false},
		{MigrationActionForward, true, DirectionUp, false},
		{MigrationActionRollback, true, DirectionDown, true},
		{MigrationAction("RESUME"), false, DirectionUp, false},
		{MigrationAction(""), false, DirectionUp, false},
	}
	for _, c := range cases {
		t.Run(string(c.action), func(t *testing.T) {
			if got := c.action.Valid(); got != c.valid {
				t.Errorf("Valid() = %v, want %v", got, c.valid)
			}
			if got := c.action.Direction(); got != c.direction {
				t.Errorf("Direction() = %s, want %s", got, c.direction)
			}
			if got := c.action.RequiresTargetExecution(); got != c.requiresTarget {
				t.Errorf("RequiresTargetExecution() = %v, want %v", got, c.requiresTarget)
			}
		})
	}
}
