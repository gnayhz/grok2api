package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestSharedMediaMarkerConcurrentSameClusterStartup(t *testing.T) {
	for round := range 32 {
		dir := t.TempDir()
		start := make(chan struct{})
		errors := make(chan error, 32)
		var workers sync.WaitGroup
		for instance := range 32 {
			workers.Go(func() {
				var cfg config.Config
				cfg.Deployment.Replicas = 32
				cfg.Deployment.ClusterID = "shared-marker-cluster"
				cfg.Deployment.InstanceID = fmt.Sprintf("instance-%d", instance)
				cfg.Media.Local.Path = dir
				<-start
				errors <- preflightDeployment(cfg)
			})
		}
		close(start)
		workers.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Errorf("round %d: same cluster startup rejected: %v", round, err)
			}
		}
		marker, err := os.ReadFile(filepath.Join(dir, sharedMediaMarkerName))
		if err != nil || string(marker) != "shared-marker-cluster\n" {
			t.Fatalf("published marker=%q err=%v", marker, err)
		}
		if t.Failed() {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("temporary files left behind: entries=%v err=%v", entries, err)
		}
	}
}

func TestSharedMediaMarkerDifferentClustersCannotOverwrite(t *testing.T) {
	dir := t.TempDir()
	start := make(chan struct{})
	type result struct {
		cluster string
		err     error
	}
	results := make(chan result, 32)
	var workers sync.WaitGroup
	for i := range 32 {
		workers.Go(func() {
			cluster := fmt.Sprintf("cluster-%d", i%2)
			cfg := config.Config{Deployment: config.DeploymentConfig{Replicas: 32, ClusterID: cluster, InstanceID: fmt.Sprint(i)}, Media: config.MediaConfig{Local: config.LocalMediaConfig{Path: dir}}}
			<-start
			results <- result{cluster, preflightDeployment(cfg)}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	marker, err := os.ReadFile(filepath.Join(dir, sharedMediaMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	winner := strings.TrimSuffix(string(marker), "\n")
	if winner != "cluster-0" && winner != "cluster-1" {
		t.Fatalf("invalid winner %q", marker)
	}
	for r := range results {
		if r.cluster == winner {
			if r.err != nil {
				t.Errorf("winning cluster rejected: %v", r.err)
			}
		} else if r.err == nil || !strings.Contains(r.err.Error(), "cluster marker mismatch") {
			t.Errorf("other cluster accepted or wrong failure: %v", r.err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left behind: entries=%v err=%v", entries, err)
	}
}

func TestSharedMediaMarkerPreservesExistingInvalidOrForeignContent(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "partial": "cluster", "foreign": "other-cluster\n", "valid": "my-cluster\n"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, sharedMediaMarkerName)
			if err := os.WriteFile(path, []byte(content), 0640); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{Deployment: config.DeploymentConfig{Replicas: 2, ClusterID: "my-cluster", InstanceID: "instance"}, Media: config.MediaConfig{Local: config.LocalMediaConfig{Path: dir}}}
			err = preflightDeployment(cfg)
			if (err == nil) != (name == "valid") {
				t.Fatalf("existing marker result: %v", err)
			}
			after, statErr := os.Stat(path)
			data, readErr := os.ReadFile(path)
			if statErr != nil || readErr != nil || string(data) != content || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("existing marker replaced: stat=%v read=%v content=%q", statErr, readErr, data)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("extra files: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestSharedMediaMarkerFailuresAndUnpublishedTemporary(t *testing.T) {
	for _, scenario := range []string{"missing_directory", "marker_is_directory", "unpublished_temporary"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if scenario == "missing_directory" {
				dir = filepath.Join(dir, "missing")
			}
			if scenario == "marker_is_directory" {
				if err := os.Mkdir(filepath.Join(dir, sharedMediaMarkerName), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var orphan string
			if scenario == "unpublished_temporary" {
				orphan = filepath.Join(dir, ".grok2api-cluster-unpublished")
				if err := os.WriteFile(orphan, []byte("incomplete"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Config{Deployment: config.DeploymentConfig{Replicas: 2, ClusterID: "my-cluster", InstanceID: "instance"}, Media: config.MediaConfig{Local: config.LocalMediaConfig{Path: dir}}}
			err := preflightDeployment(cfg)
			if (err == nil) != (scenario == "unpublished_temporary") {
				t.Fatalf("preflight result: %v", err)
			}
			if scenario == "unpublished_temporary" {
				marker, err := os.ReadFile(filepath.Join(dir, sharedMediaMarkerName))
				if err != nil || string(marker) != "my-cluster\n" {
					t.Fatalf("committed marker=%q err=%v", marker, err)
				}
				left, err := os.ReadFile(orphan)
				if err != nil || string(left) != "incomplete" {
					t.Fatalf("foreign temporary was changed: %q %v", left, err)
				}
			}
		})
	}
}
