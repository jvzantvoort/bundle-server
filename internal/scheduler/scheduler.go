package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Job struct {
	Name       string
	Interval   time.Duration
	RunOnStart bool
	Run        func(context.Context)
}

type Scheduler struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func Start(parent context.Context, jobs []Job) (*Scheduler, error) {
	ctx, cancel := context.WithCancel(parent)
	scheduler := &Scheduler{cancel: cancel}
	for _, job := range jobs {
		if err := validate(job); err != nil {
			cancel()
			return nil, err
		}
		scheduler.wg.Add(1)
		go scheduler.run(ctx, job)
	}
	return scheduler, nil
}

func (s *Scheduler) Stop() {
	s.cancel()
	s.wg.Wait()
}

func validate(job Job) error {
	if job.Name == "" {
		return fmt.Errorf("job name is required")
	}
	if job.Interval <= 0 {
		return fmt.Errorf("job %q interval must be positive", job.Name)
	}
	if job.Run == nil {
		return fmt.Errorf("job %q runner is required", job.Name)
	}
	return nil
}

func (s *Scheduler) run(ctx context.Context, job Job) {
	defer s.wg.Done()
	if job.RunOnStart {
		job.Run(ctx)
	}
	ticker := time.NewTicker(job.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job.Run(ctx)
		}
	}
}
