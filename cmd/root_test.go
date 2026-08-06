package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/difyz9/dmon-cli/internal/checkpoint"
)

func TestAliasesAndDefaultStatePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	for _, test := range []struct {
		args   []string
		action string
	}{
		{[]string{"wait", "--timeout", "1s"}, "next"}, {[]string{"ack", "id"}, "done"}, {[]string{"nack", "id"}, "retry"},
	} {
		root, opts, err := newRoot(test.args, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if opts.action != test.action {
			t.Fatalf("%v action=%s", test.args, opts.action)
		}
		if opts.checkpoint != filepath.Join(stateHome, "dmon", "state.db") {
			t.Fatalf("checkpoint=%s", opts.checkpoint)
		}
	}
}

func TestNextReturnsBusyWhileClaimIsInflight(t *testing.T) {
	root := t.TempDir()
	watch := filepath.Join(root, "in")
	if err := os.MkdirAll(watch, 0o755); err != nil {
		t.Fatal(err)
	}
	config := []byte("watch: " + watch + "\nscan_interval: 10ms\ninactive: 0s\ncheckpoint: " + filepath.Join(root, "state.db") + "\ninclude: '\\.csv$'\nexclude: ''\nqueue:\n  max_inflight: 1\n  retry_delay: 1s\n  max_attempts: 3\n")
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(watch, "a.csv"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(watch, "b.csv"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	var first bytes.Buffer
	if err := execute(context.Background(), []string{"next", "--config", configPath, "--timeout", "1s"}, &first, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), []string{"next", "--config", configPath, "--timeout", "1s"}, &bytes.Buffer{}, &bytes.Buffer{}); !errors.Is(err, checkpoint.ErrQueueBusy) {
		t.Fatalf("second next error=%v", err)
	}
}

func TestNextExecEndToEnd(t *testing.T) {
	root := t.TempDir()
	watch := filepath.Join(root, "in")
	archive := filepath.Join(root, "archive")
	os.MkdirAll(watch, 0o755)
	config := []byte("watch: " + watch + "\narchive_dir: " + archive + "\nscan_interval: 10ms\ninactive: 0s\ncheckpoint: " + filepath.Join(root, "state.db") + "\ninclude: '\\.csv$'\nexclude: ''\n")
	configPath := filepath.Join(root, "config.yaml")
	os.WriteFile(configPath, config, 0o600)
	os.WriteFile(filepath.Join(watch, "a.csv"), []byte("x"), 0o644)
	var stdout, stderr bytes.Buffer
	err := execute(context.Background(), []string{"next", "--config", configPath, "--timeout", "1s", "--exec", `test -f "$DIRWATCH_FILE_PATH"`}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("execute: %v stderr=%s", err, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"status":"acknowledged"`)) {
		t.Fatalf("stdout=%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(archive, "a.csv")); err != nil {
		t.Fatal(err)
	}
}
