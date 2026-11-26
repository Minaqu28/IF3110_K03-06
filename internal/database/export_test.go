package database

func (e *TargetExecutor) SetBeforeCommit(hook func(pid uint32)) { e.beforeCommit = hook }
