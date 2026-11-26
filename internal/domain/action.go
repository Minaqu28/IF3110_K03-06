package domain

type Direction string

const (
	DirectionUp   Direction = "UP"
	DirectionDown Direction = "DOWN"
)

func (a MigrationAction) Valid() bool {
	switch a {
	case MigrationActionApply, MigrationActionRollback, MigrationActionForward:
		return true
	}
	return false
}

func (a MigrationAction) Direction() Direction {
	if a == MigrationActionRollback {
		return DirectionDown
	}
	return DirectionUp
}

func (a MigrationAction) RequiresTargetExecution() bool {
	return a == MigrationActionRollback
}
