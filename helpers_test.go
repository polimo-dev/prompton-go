package prompton

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A frozen clock keeps the retry and cache windows deterministic: the tests
// advance time explicitly instead of sleeping through it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func quietLogger(t *testing.T) func(string, ...interface{}) {
	return func(format string, args ...interface{}) {
		t.Logf("sdk: "+format, args...)
	}
}

const testSnapshotJSON = `{
  "schema_version": 3,
  "project": "sdkfixture",
  "environment": "production",
  "use_cases": {
    "greeting": {
      "id": "0198f2a1-0000-7000-8000-00000000c001",
      "kind": "chat",
      "input_schema": [{"name": "name", "type": "string", "required": true}],
      "default_params": {"max_tokens": 512, "temperature": 0.7},
      "payload_policy": {"mode": "full", "sample_rate": 1.0, "max_bytes": 262144, "retention_days": 30, "encrypt": false}
    }
  },
  "deployments": {
    "greeting": {
      "id": "0198f2a1-0000-7000-8000-00000000d001",
      "revision": 3,
      "model_id": "0198f2a1-0000-7000-8000-00000000e001",
      "params": {"temperature": 0.2},
      "provider_options": {},
      "prompt_pins": {"default": "0198f2a1-0000-7000-8000-00000000a001"}
    }
  },
  "prompt_versions": {
    "0198f2a1-0000-7000-8000-00000000a001": {
      "id": "0198f2a1-0000-7000-8000-00000000a001",
      "prompt_id": "0198f2a1-0000-7000-8000-00000000b001",
      "number": 2,
      "engine": "liquid",
      "messages": [{"role": "system", "content": "You greet people."}, {"role": "user", "content": "Say hello to {{ name }}."}],
      "text_template": null
    }
  },
  "models": {
    "0198f2a1-0000-7000-8000-00000000e001": {
      "id": "0198f2a1-0000-7000-8000-00000000e001",
      "provider": "openrouter",
      "model_id": "openai/gpt-4o-mini",
      "display_name": "GPT-4o mini",
      "metadata": {},
      "provider_options": {"only": ["OpenAI"]},
      "capabilities": ["tools"],
      "status": "active"
    }
  }
}`

func stagingSnapshotJSON() string {
	return `{"schema_version":3,"project":"sdkfixture","environment":"staging","use_cases":{},"deployments":{},"prompt_versions":{},"models":{}}`
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// waitFor polls a condition so a test never depends on a fixed sleep.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustResolve(t *testing.T, c *Client, useCase string, opts ...ResolveOption) *Resolution {
	t.Helper()
	res, err := c.Resolve(testContext(t), useCase, opts...)
	if err != nil {
		t.Fatalf("resolve %s: %v", useCase, err)
	}
	return res
}

func sampleRecord(i int) GenerationRecord {
	return GenerationRecord{
		UseCase:   "greeting",
		Model:     "openai/gpt-4o-mini",
		Status:    StatusOK,
		StartedAt: time.Now().UTC(),
		TraceID:   fmt.Sprintf("test:%d", i),
	}
}
