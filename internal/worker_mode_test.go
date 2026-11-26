package main

import (
	"strings"
	"testing"
	"time"
)

func TestWorkerConfigFromEnv(t *testing.T) {
	t.Setenv("AMQP_URL", "amqp://u:p@broker:5672/")
	t.Setenv("AMQP_QUEUE", "")
	t.Setenv("WORKER_SLOTS", "")
	t.Setenv("MIGRATION_BATCH_TIMEOUT", "")
	t.Setenv("LOCK_LEASE", "")
	c, err := workerConfigFromEnv()
	if err != nil || c.queue != defaultJobQueue || c.slots != 4 || c.batchTimeout != 5*time.Minute || c.lockLease != 30*time.Second {
		t.Fatalf("defaults = %+v, err = %v", c, err)
	}

	t.Setenv("AMQP_QUEUE", "jobs.test")
	t.Setenv("WORKER_SLOTS", "8")
	t.Setenv("MIGRATION_BATCH_TIMEOUT", "90s")
	t.Setenv("LOCK_LEASE", "1m")
	if c, err := workerConfigFromEnv(); err != nil || c.queue != "jobs.test" || c.slots != 8 || c.batchTimeout != 90*time.Second || c.lockLease != time.Minute {
		t.Fatalf("overrides = %+v, err = %v", c, err)
	}

	for name, env := range map[string][2]string{
		"no broker":             {"AMQP_URL", ""},
		"zero slots":            {"WORKER_SLOTS", "0"},
		"slots not a number":    {"WORKER_SLOTS", "four"},
		"bad batch timeout":     {"MIGRATION_BATCH_TIMEOUT", "5 minutes"},
		"negative lease":        {"LOCK_LEASE", "-30s"},
		"lease under heartbeat": {"LOCK_LEASE", "5s"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env[0], env[1])
			if _, err := workerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), env[0]) {
				t.Fatalf("err = %v, want one naming %s", err, env[0])
			}
		})
	}
}

func TestWorkerIDIsUnique(t *testing.T) {
	a, b := workerID(), workerID()
	if a == b || strings.Count(a, ":") != 2 {
		t.Fatalf("worker ids %q and %q", a, b)
	}
}
