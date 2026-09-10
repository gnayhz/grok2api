package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestSharedMediaMarkerAcrossProcesses(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed_clusters_%t", mixed), func(t *testing.T) {
			dir, coordination := t.TempDir(), t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			type process struct {
				cmd     *exec.Cmd
				output  bytes.Buffer
				cluster string
				waited  bool
			}
			children := make([]*process, 0, 4)
			defer func() {
				cancel()
				for _, child := range children {
					if !child.waited {
						_ = child.cmd.Wait()
					}
				}
			}()
			for i := range 4 {
				cluster := "cluster-0"
				if mixed {
					cluster = fmt.Sprintf("cluster-%d", i%2)
				}
				child := &process{cluster: cluster}
				child.cmd = exec.CommandContext(ctx, executable, "-test.run=^TestSharedMediaMarkerProcessChild$", "-test.timeout=20s")
				child.cmd.Env = append(os.Environ(), "GROK_TEST_MARKER_DIRECTORY="+dir, "GROK_TEST_MARKER_COORDINATION="+coordination, "GROK_TEST_MARKER_INSTANCE="+fmt.Sprint(i), "GROK_TEST_MARKER_CLUSTER="+cluster)
				child.cmd.Stdout, child.cmd.Stderr = &child.output, &child.output
				if err := child.cmd.Start(); err != nil {
					t.Fatal(err)
				}
				children = append(children, child)
			}
			for i := range children {
				for {
					if _, err := os.Stat(filepath.Join(coordination, fmt.Sprintf("ready-%d", i))); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("marker child did not reach startup barrier")
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
			if err := os.WriteFile(filepath.Join(coordination, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				err := child.cmd.Wait()
				child.waited = true
				if err != nil {
					t.Fatalf("marker process: %v\n%s", err, child.output.String())
				}
			}
			marker, err := os.ReadFile(filepath.Join(dir, sharedMediaMarkerName))
			if err != nil {
				t.Fatal(err)
			}
			winner := strings.TrimSuffix(string(marker), "\n")
			if winner != "cluster-0" && (!mixed || winner != "cluster-1") {
				t.Fatalf("invalid marker=%q", marker)
			}
			for i, child := range children {
				result, err := os.ReadFile(filepath.Join(coordination, fmt.Sprintf("result-%d", i)))
				if err != nil {
					t.Fatal(err)
				}
				if child.cluster == winner {
					if len(result) != 0 {
						t.Errorf("winning cluster failed: %s", result)
					}
				} else if !strings.Contains(string(result), "cluster marker mismatch") {
					t.Errorf("other cluster result=%q", result)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary files left behind: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestSharedMediaMarkerProcessChild(t *testing.T) {
	dir := os.Getenv("GROK_TEST_MARKER_DIRECTORY")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	coordination, instance := os.Getenv("GROK_TEST_MARKER_COORDINATION"), os.Getenv("GROK_TEST_MARKER_INSTANCE")
	if err := os.WriteFile(filepath.Join(coordination, "ready-"+instance), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		if _, err := os.Stat(filepath.Join(coordination, "release")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("parent did not release startup barrier")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cfg := config.Config{Deployment: config.DeploymentConfig{Replicas: 4, ClusterID: os.Getenv("GROK_TEST_MARKER_CLUSTER"), InstanceID: instance}, Media: config.MediaConfig{Local: config.LocalMediaConfig{Path: dir}}}
	result := ""
	if err := preflightDeployment(cfg); err != nil {
		result = err.Error()
	}
	if err := os.WriteFile(filepath.Join(coordination, "result-"+instance), []byte(result), 0600); err != nil {
		t.Fatal(err)
	}
}
