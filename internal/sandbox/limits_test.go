package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLimitsRejectEveryUnboundedOrUnsupportedPolicy(t *testing.T) {
	for _, limits := range []Limits{
		DefaultLimits(),
		{128 << 20, 1, 1, 1024, time.Second},
		{2 << 30, 60, 1 << 40, 8 << 20, time.Minute},
	} {
		if err := limits.Validate(); err != nil {
			t.Fatal("valid policy rejected", limits, err)
		}
	}
	for name, change := range map[string]func(*Limits){
		"small memory": func(l *Limits) { l.MemoryBytes = (128 << 20) - 1 },
		"large memory": func(l *Limits) { l.MemoryBytes = (2 << 30) + 1 },
		"zero cpu":     func(l *Limits) { l.CPUSeconds = 0 },
		"large cpu":    func(l *Limits) { l.CPUSeconds = 61 },
		"zero input":   func(l *Limits) { l.InputBytes = 0 },
		"large input":  func(l *Limits) { l.InputBytes = (1 << 40) + 1 },
		"small output": func(l *Limits) { l.OutputBytes = 1023 },
		"large output": func(l *Limits) { l.OutputBytes = (8 << 20) + 1 },
		"short time":   func(l *Limits) { l.Timeout = time.Second - 1 },
		"long time":    func(l *Limits) { l.Timeout = time.Minute + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			limits := DefaultLimits()
			change(&limits)
			if err := limits.Validate(); !errors.Is(err, ErrLimits) {
				t.Fatal("unsafe policy accepted", limits, err)
			}
		})
	}
}

func TestBoundedOutputCancelsWithoutRetainingExcessBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &boundedOutput{limit: 4, cancel: cancel}
	for _, data := range []string{"ab", "cd"} {
		if n, err := output.Write([]byte(data)); n != 2 || err != nil {
			t.Fatal("bounded write failed", n, err)
		}
	}
	if ctx.Err() != nil || output.exceeded.Load() {
		t.Fatal("exact limit cancelled output")
	}
	if n, err := output.Write([]byte("secret excess")); n != 0 || !errors.Is(err, ErrResource) || !output.exceeded.Load() || !errors.Is(ctx.Err(), context.Canceled) || output.data.String() != "abcd" {
		t.Fatal("overflow was retained, not bounded or not cancelled", n, err)
	}
}
