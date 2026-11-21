package taskqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

type Task struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Status     Status    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Result     any       `json:"result,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type Queue interface {
	Enqueue(context.Context, string, func(context.Context) (any, error)) (Task, error)
	Get(string) (Task, bool)
	Close()
}

// DefaultMaxPending is how many tasks can wait for a worker before Enqueue
// refuses new ones.
const DefaultMaxPending = 1000

var (
	// ErrClosed is returned by Enqueue after Close.
	ErrClosed = errors.New("task queue is closed")
	// ErrFull is returned by Enqueue when too many tasks are waiting.
	ErrFull = errors.New("task queue is full")
)

// MemoryQueue runs tasks on a fixed number of workers. Enqueue never waits for
// a worker: tasks wait in a bounded list until one is free.
type MemoryQueue struct {
	mu         sync.Mutex
	ready      *sync.Cond // signalled when a task is added or the queue closes
	tasks      map[string]Task
	pending    []job
	maxPending int
	closed     bool
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	nextID     func() string
}

type job struct {
	id  string
	run func(context.Context) (any, error)
}

func NewMemoryQueue(workers int, nextID func() string) *MemoryQueue {
	if workers < 1 {
		workers = 1
	}
	if nextID == nil {
		var sequence uint64
		var mu sync.Mutex
		nextID = func() string {
			mu.Lock()
			defer mu.Unlock()
			sequence++
			return fmt.Sprintf("task-%d", sequence)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &MemoryQueue{tasks: make(map[string]Task), maxPending: DefaultMaxPending, ctx: ctx, cancel: cancel, nextID: nextID}
	q.ready = sync.NewCond(&q.mu)
	for range workers {
		q.wg.Add(1)
		go q.worker()
	}
	return q
}

// Enqueue adds a task and returns at once. It returns ErrFull when too many
// tasks are waiting, and ErrClosed after Close.
func (q *MemoryQueue) Enqueue(_ context.Context, taskType string, run func(context.Context) (any, error)) (Task, error) {
	if run == nil {
		return Task{}, fmt.Errorf("task runner is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return Task{}, ErrClosed
	}
	if len(q.pending) >= q.maxPending {
		return Task{}, ErrFull
	}
	task := Task{ID: q.nextID(), Type: taskType, Status: StatusQueued, CreatedAt: time.Now().UTC()}
	q.tasks[task.ID] = task
	q.pending = append(q.pending, job{id: task.ID, run: run})
	q.ready.Signal()
	return task, nil
}

func (q *MemoryQueue) Get(id string) (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	task, ok := q.tasks[id]
	return task, ok
}

// Close cancels running tasks, fails the ones still waiting, and waits for the
// workers to stop. It can be called more than once.
func (q *MemoryQueue) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		now := time.Now().UTC()
		for _, job := range q.pending {
			task := q.tasks[job.id]
			task.Status = StatusFailed
			task.Error = ErrClosed.Error()
			task.FinishedAt = now
			q.tasks[job.id] = task
		}
		q.pending = nil
		q.ready.Broadcast()
	}
	q.mu.Unlock()
	q.cancel()
	q.wg.Wait()
}

func (q *MemoryQueue) worker() {
	defer q.wg.Done()
	for {
		job, ok := q.next()
		if !ok {
			return
		}
		result, err := job.run(q.ctx)
		q.update(job.id, func(task *Task) {
			task.FinishedAt = time.Now().UTC()
			if err != nil {
				task.Status = StatusFailed
				task.Error = err.Error()
				return
			}
			task.Status = StatusDone
			task.Result = result
		})
	}
}

// next waits for a task and marks it running. It returns false once the queue
// is closed.
func (q *MemoryQueue) next() (job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.pending) == 0 && !q.closed {
		q.ready.Wait()
	}
	if q.closed {
		return job{}, false
	}
	next := q.pending[0]
	q.pending[0] = job{} // let the runner be collected
	q.pending = q.pending[1:]
	task := q.tasks[next.id]
	task.Status = StatusRunning
	task.StartedAt = time.Now().UTC()
	q.tasks[next.id] = task
	return next, true
}

func (q *MemoryQueue) update(id string, update func(*Task)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	task := q.tasks[id]
	update(&task)
	q.tasks[id] = task
}
