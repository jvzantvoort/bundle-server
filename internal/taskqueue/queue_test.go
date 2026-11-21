package taskqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// wait polls until the task reaches a final status.
func wait(t *testing.T, q *MemoryQueue, id string) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := q.Get(id)
		if !ok {
			t.Fatalf("task %s does not exist", id)
		}
		if task.Status == StatusDone || task.Status == StatusFailed {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", id)
	return Task{}
}

func TestTaskSucceeds(t *testing.T) {
	q := NewMemoryQueue(1, nil)
	defer q.Close()

	queued, err := q.Enqueue(context.Background(), "verify", func(context.Context) (any, error) { return "ok", nil })
	if err != nil {
		t.Fatal(err)
	}
	if queued.Type != "verify" || queued.CreatedAt.IsZero() {
		t.Fatalf("queued task = %+v", queued)
	}

	task := wait(t, q, queued.ID)
	if task.Status != StatusDone || task.Result != "ok" || task.Error != "" {
		t.Fatalf("task = %+v, want done with result ok", task)
	}
	if task.StartedAt.IsZero() || task.FinishedAt.Before(task.StartedAt) {
		t.Fatalf("task times = started %v, finished %v", task.StartedAt, task.FinishedAt)
	}
}

func TestTaskFails(t *testing.T) {
	q := NewMemoryQueue(1, nil)
	defer q.Close()

	queued, err := q.Enqueue(context.Background(), "verify", func(context.Context) (any, error) { return "partial", errors.New("disk error") })
	if err != nil {
		t.Fatal(err)
	}
	task := wait(t, q, queued.ID)
	if task.Status != StatusFailed || task.Error != "disk error" || task.Result != nil {
		t.Fatalf("task = %+v, want failed with error and no result", task)
	}
}

func TestEnqueueRequiresRunner(t *testing.T) {
	q := NewMemoryQueue(1, nil)
	defer q.Close()

	if _, err := q.Enqueue(context.Background(), "verify", nil); err == nil {
		t.Fatal("Enqueue accepted a nil runner")
	}
}

func TestTaskIDsAreUniqueAndUnknownIDsAreMissing(t *testing.T) {
	q := NewMemoryQueue(2, nil)
	defer q.Close()

	seen := make(map[string]bool)
	for range 20 {
		task, err := q.Enqueue(context.Background(), "noop", func(context.Context) (any, error) { return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		if seen[task.ID] {
			t.Fatalf("duplicate task id %s", task.ID)
		}
		seen[task.ID] = true
	}
	for id := range seen {
		wait(t, q, id)
	}
	if _, ok := q.Get("no-such-task"); ok {
		t.Fatal("Get found a task that was never queued")
	}
}

func TestCustomIDs(t *testing.T) {
	q := NewMemoryQueue(1, func() string { return "fixed" })
	defer q.Close()

	task, err := q.Enqueue(context.Background(), "noop", func(context.Context) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "fixed" {
		t.Fatalf("task id = %q, want fixed", task.ID)
	}
	wait(t, q, task.ID)
}

func TestWorkersRunConcurrently(t *testing.T) {
	// Fewer than one worker means one
	if q := NewMemoryQueue(0, nil); q != nil {
		q.Close()
	}

	q := NewMemoryQueue(2, nil)
	defer q.Close()

	// Each task waits for the other, so they only finish when both run at once
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	run := func(context.Context) (any, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}
	first, _ := q.Enqueue(context.Background(), "a", run)
	second, _ := q.Enqueue(context.Background(), "b", run)
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("two workers did not run two tasks at once")
		}
	}
	close(release)
	wait(t, q, first.ID)
	wait(t, q, second.ID)
}

func TestCloseCancelsRunningTask(t *testing.T) {
	q := NewMemoryQueue(1, nil)

	cancelled := make(chan struct{})
	running := make(chan struct{})
	if _, err := q.Enqueue(context.Background(), "long", func(ctx context.Context) (any, error) {
		close(running)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	<-running

	done := make(chan struct{})
	go func() {
		q.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("running task was not cancelled")
	}
}
