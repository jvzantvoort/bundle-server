package taskqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// occupy runs a task on the queue's only worker until release is closed.
func occupy(t *testing.T, q *MemoryQueue) (release chan struct{}) {
	t.Helper()
	release = make(chan struct{})
	running := make(chan struct{})
	if _, err := q.Enqueue(context.Background(), "busy", func(context.Context) (any, error) {
		close(running)
		<-release
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start the task")
	}
	return release
}

// enqueueNow fails the test when Enqueue does not return promptly.
func enqueueNow(t *testing.T, q *MemoryQueue, run func(context.Context) (any, error)) (Task, error) {
	t.Helper()
	type result struct {
		task Task
		err  error
	}
	done := make(chan result, 1)
	go func() {
		task, err := q.Enqueue(context.Background(), "noop", run)
		done <- result{task, err}
	}()
	select {
	case r := <-done:
		return r.task, r.err
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked")
		return Task{}, nil
	}
}

func finished(t *testing.T, q *MemoryQueue, id string) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if task, _ := q.Get(id); task.Status == StatusDone || task.Status == StatusFailed {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", id)
	return Task{}
}

func TestEnqueueDoesNotWaitForBusyWorker(t *testing.T) {
	q := NewMemoryQueue(1, nil)
	defer q.Close()
	release := occupy(t, q)

	var order []string
	record := make(chan string, 3)
	var ids []string
	for _, name := range []string{"first", "second", "third"} {
		task, err := enqueueNow(t, q, func(context.Context) (any, error) { record <- name; return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := q.Get(task.ID); got.Status != StatusQueued {
			t.Fatalf("waiting task status = %q, want queued", got.Status)
		}
		ids = append(ids, task.ID)
	}

	close(release)
	for _, id := range ids {
		if task := finished(t, q, id); task.Status != StatusDone {
			t.Fatalf("task %s = %+v, want done", id, task)
		}
	}
	for range ids {
		order = append(order, <-record)
	}
	if order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Fatalf("tasks ran in order %v, want first, second, third", order)
	}
}

func TestEnqueueRefusesWhenFull(t *testing.T) {
	q := NewMemoryQueue(1, nil)
	defer q.Close()
	q.maxPending = 2
	release := occupy(t, q)
	defer close(release)

	noop := func(context.Context) (any, error) { return nil, nil }
	for range 2 {
		if _, err := enqueueNow(t, q, noop); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enqueueNow(t, q, noop); !errors.Is(err, ErrFull) {
		t.Fatalf("Enqueue on a full queue = %v, want ErrFull", err)
	}
}

func TestCloseFailsWaitingTasksAndRefusesNewOnes(t *testing.T) {
	q := NewMemoryQueue(1, nil)
	release := occupy(t, q)

	ran := make(chan struct{}, 1)
	waiting, err := enqueueNow(t, q, func(context.Context) (any, error) { ran <- struct{}{}; return nil, nil })
	if err != nil {
		t.Fatal(err)
	}

	// The busy task ignores cancellation, so Close waits for it
	closed := make(chan struct{})
	go func() {
		q.Close()
		close(closed)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if task, _ := q.Get(waiting.ID); task.Status == StatusFailed {
			if task.Error != ErrClosed.Error() || task.FinishedAt.IsZero() {
				t.Fatalf("waiting task after Close = %+v", task)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not fail the waiting task")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := enqueueNow(t, q, func(context.Context) (any, error) { return nil, nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after Close = %v, want ErrClosed", err)
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-ran:
		t.Fatal("a task that was waiting at Close still ran")
	default:
	}
	q.Close() // a second Close returns at once
}
