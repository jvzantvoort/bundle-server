package taskqueue

import (
	"context"
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

type MemoryQueue struct {
	mu     sync.RWMutex
	tasks  map[string]Task
	work   chan job
	cancel context.CancelFunc
	wg     sync.WaitGroup
	nextID func() string
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
	q := &MemoryQueue{tasks: make(map[string]Task), work: make(chan job), cancel: cancel, nextID: nextID}
	for range workers {
		q.wg.Add(1)
		go q.worker(ctx)
	}
	return q
}

func (q *MemoryQueue) Enqueue(_ context.Context, taskType string, run func(context.Context) (any, error)) (Task, error) {
	if run == nil {
		return Task{}, fmt.Errorf("task runner is required")
	}
	task := Task{ID: q.nextID(), Type: taskType, Status: StatusQueued, CreatedAt: time.Now().UTC()}
	q.mu.Lock()
	q.tasks[task.ID] = task
	q.mu.Unlock()
	q.work <- job{id: task.ID, run: run}
	return task, nil
}

func (q *MemoryQueue) Get(id string) (Task, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	task, ok := q.tasks[id]
	return task, ok
}

func (q *MemoryQueue) Close() {
	q.cancel()
	q.wg.Wait()
}

func (q *MemoryQueue) worker(ctx context.Context) {
	defer q.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-q.work:
			q.update(job.id, func(task *Task) {
				task.Status = StatusRunning
				task.StartedAt = time.Now().UTC()
			})
			result, err := job.run(ctx)
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
}

func (q *MemoryQueue) update(id string, update func(*Task)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	task := q.tasks[id]
	update(&task)
	q.tasks[id] = task
}
