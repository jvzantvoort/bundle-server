package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestStartRunsImmediatelyAndRepeats(t *testing.T) {
	runs := make(chan struct{}, 2)
	scheduler, err := Start(context.Background(), []Job{{
		Name: "test", Interval: 5 * time.Millisecond, RunOnStart: true,
		Run: func(context.Context) { runs <- struct{}{} },
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()

	for range 2 {
		select {
		case <-runs:
		case <-time.After(time.Second):
			t.Fatal("scheduled job did not run")
		}
	}
}

func TestStartRejectsInvalidJob(t *testing.T) {
	if _, err := Start(context.Background(), []Job{{Name: "bad"}}); err == nil {
		t.Fatal("Start accepted an invalid job")
	}
}
