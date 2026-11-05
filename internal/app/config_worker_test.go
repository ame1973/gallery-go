package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/media"
	"gallerygo/internal/scanner"
)

func TestWorkerAndScanResourceDefaultsAndStrictOverrides(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "sources: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Worker.Embedded || !c.Worker.Required || c.Worker.Policy != jobs.DefaultPolicy() || c.Scanner != scanner.DefaultPolicy() || c.Inspection != media.DefaultInspectPolicy() || c.Scheduler != scanner.DefaultSchedulePolicy() {
		t.Fatal("worker safety defaults changed", c.Worker, c.Scanner, c.Inspection)
	}
	c, err = LoadConfig(writeConfig(t, `worker:
  embedded: false
  required: false
  concurrency: 3
  lease: 6s
  heartbeat: 1s
  poll: 100ms
  max_attempts: 2
  retry_base: 200ms
  retry_max: 2s
scanner:
  max_depth: 7
  max_files: 100
  max_file_bytes: 1024
  batch_size: 4
  timeout: 2m
inspection:
  max_pixels: 999
  max_dimension: 22
  max_frames: 3
  max_duration: 2s
  header_bytes: 512
  timeout: 1s
scheduler:
  enabled: false
  interval: 2s
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Worker.Embedded || c.Worker.Required || c.Worker.Concurrency != 3 || c.Worker.Lease != 6*time.Second || c.Worker.Heartbeat != time.Second || c.Worker.Poll != 100*time.Millisecond || c.Worker.MaxAttempts != 2 || c.Worker.RetryBase != 200*time.Millisecond || c.Worker.RetryMax != 2*time.Second || c.Scanner.MaxDepth != 7 || c.Scanner.MaxFiles != 100 || c.Scanner.MaxFileBytes != 1024 || c.Scanner.BatchSize != 4 || c.Scanner.Timeout != 2*time.Minute || c.Inspection.MaxPixels != 999 || c.Inspection.MaxDimension != 22 || c.Inspection.MaxFrames != 3 || c.Inspection.MaxDuration != 2*time.Second || c.Inspection.HeaderBytes != 512 || c.Inspection.Timeout != time.Second || c.Scheduler.Enabled || c.Scheduler.Interval != 2*time.Second {
		t.Fatal(c.Worker, c.Scanner, c.Inspection)
	}
	if _, err := os.Stat(filepath.Dir(c.Database)); !os.IsNotExist(err) {
		t.Fatal("loading worker policy created runtime data", err)
	}
}

func TestWorkerAndScanConfigurationRejectsUnsafeOrAmbiguousPolicy(t *testing.T) {
	cases := []string{
		"worker: {required: null}", "worker: {required: arbitrary}", "worker: {embedded: true, required: false}",
		"worker: null", "worker: {embedded: null}", "worker: {embedded: arbitrary}", "worker: {concurrency: 0}", "worker: {concurrency: 17}",
		"worker: {lease: 2s}", "worker: {lease: 11m}", "worker: {heartbeat: 0s}", "worker: {lease: 3s, heartbeat: 2s}", "worker: {poll: 0s}",
		"worker: {max_attempts: 0}", "worker: {max_attempts: 11}", "worker: {retry_base: 0s}", "worker: {retry_base: 2s, retry_max: 1s}",
		"worker: {unknown: 1}", "worker: {concurrency: 2, concurrency: 3}", "worker: {lease: 30000000000}",
		"scanner: null", "scanner: {max_depth: 0}", "scanner: {max_depth: 257}", "scanner: {max_files: 0}", "scanner: {max_file_bytes: 0}",
		"scanner: {batch_size: 0}", "scanner: {batch_size: 257}", "scanner: {timeout: 0s}", "scanner: {unknown: 1}",
		"inspection: null", "inspection: {max_pixels: 0}", "inspection: {max_dimension: 0}", "inspection: {header_bytes: 511}",
		"inspection: {max_frames: 0}", "inspection: {max_frames: 100001}", "inspection: {max_frames: null}",
		"inspection: {max_duration: 0s}", "inspection: {max_duration: 1ns}", "inspection: {max_duration: 25h}", "inspection: {max_duration: null}", "inspection: {max_duration: 1000000000}",
		"inspection: {timeout: 0s}", "inspection: {timeout: 61s}", "inspection: {max_pixels: null}", "inspection: {unknown: 1}",
		"auth: null", "sources: null", "null", "worker: &policy {embedded: false}", "worker: &policy {embedded: false}\ninspection: *policy",
		"worker: {<<: {embedded: false}}", "{1: false}",
		"scheduler: null", "scheduler: {enabled: null}", "scheduler: {enabled: arbitrary}", "scheduler: {interval: 999ms}",
		"scheduler: {interval: 61s}", "scheduler: {interval: null}", "scheduler: {interval: 1000000000}", "scheduler: {unknown: 1}",
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, text)); err == nil {
				t.Fatal("unsafe runtime configuration accepted", text)
			}
		})
	}
}
