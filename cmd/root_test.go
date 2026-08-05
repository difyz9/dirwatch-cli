package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
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
		if opts.checkpoint != filepath.Join(stateHome, "dirwatch-cli", "state.db") {
			t.Fatalf("checkpoint=%s", opts.checkpoint)
		}
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
