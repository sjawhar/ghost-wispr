package config

import (
	"path/filepath"
	"testing"
)

// TestSummaryBatchJobsAreAConfigChoice fails if a unit summarizes a backlog
// with batch jobs when its config says not to, or stops using them when its
// config says nothing: a unit whose Google project refuses every Vertex AI
// batch job create (the office Pi's does) must summarize each session online.
func TestSummaryBatchJobsAreAConfigChoice(t *testing.T) {
	clearVoiceEnv(t)
	cfg, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Summarization.BatchJobs {
		t.Error("a config that says nothing turned summary batch jobs off; want them on, as before")
	}

	cfg, _, err = Load(writeConfig(t, "summarization:\n  model: openai/gpt-example\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Summarization.BatchJobs {
		t.Error("a summarization section without batch_jobs turned batch jobs off")
	}

	cfg, _, err = Load(writeConfig(t, "summarization:\n  batch_jobs: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Summarization.BatchJobs {
		t.Error("summarization.batch_jobs: false left batch jobs on")
	}

	pi, _, err := Load(filepath.Join("..", "..", "deploy", "pi", "files", "ghost-wispr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if pi.Summarization.BatchJobs {
		t.Error("the office Pi's config leaves summary batch jobs on; its project refuses them")
	}
}
