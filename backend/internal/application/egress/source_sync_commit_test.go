package egress

import (
	"context"
	netfetch "github.com/chenyme/grok2api/backend/internal/testsupport/netfetch"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestSourceSyncCommitRejectsRetiredFetchConfiguration(t *testing.T) {
	for _, change := range []string{"deleted", "url_changed", "proxy_changed", "renamed", "disabled", "interval_changed"} {
		for _, responseFailure := range []bool{false, true} {
			name := change + "/success"
			if responseFailure {
				name = change + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				ctx, service, repo := newFinalReviewHealthFixture(t)
				defer service.Close(ctx)
				bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				ready, released := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(released) }) }
				defer release()
				feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Host != "1.1.1.1" {
						http.Error(w, "unexpected target", 502)
						return
					}
					close(ready)
					select {
					case <-released:
					case <-r.Context().Done():
						return
					}
					if responseFailure {
						http.Error(w, "old fetch failed", 503)
						return
					}
					_, _ = io.WriteString(w, "http://old-feed.example:8080\n")
				}))
				defer feed.Close()
				publicURL := "http://1.1.1.1/old"
				proxyURL := feed.URL
				source, err := service.CreateSource(ctx, SubscriptionSourceInput{Name: t.Name(), Enabled: true, URL: &publicURL, ProxyURL: &proxyURL})
				if err != nil {
					t.Fatal(err)
				}
				defer service.DeleteSource(ctx, source.ID)
				done := make(chan error, 1)
				go func() { _, err := service.SyncSource(bounded, source.ID); done <- err }()
				select {
				case <-ready:
				case err := <-done:
					t.Fatalf("sync failed before fetch: %v", err)
				case <-bounded.Done():
					t.Fatal(bounded.Err())
				}
				var before domain.SubscriptionSource
				if change == "deleted" {
					if err := service.DeleteSource(ctx, source.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					input := SubscriptionSourceInput{Name: source.Name, Enabled: true}
					switch change {
					case "renamed":
						input.Name += "-new"
					case "disabled":
						input.Enabled = false
					case "interval_changed":
						interval := 1800
						input.RefreshIntervalSeconds = &interval
					case "url_changed":
						url := "http://1.1.1.1/new"
						input.URL = &url
					case "proxy_changed":
						url := "http://127.0.0.1:1"
						input.ProxyURL = &url
					}
					if _, err := service.UpdateSource(ctx, source.ID, input); err != nil {
						t.Fatal(err)
					}
					before, err = repo.GetEgressSource(ctx, source.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				release()
				syncErr := <-done
				t.Logf("source=%d change=%s responseFailure=%v result=%v", source.ID, change, responseFailure, syncErr)
				if syncErr == nil {
					t.Errorf("retired %s fetch was accepted", change)
				}
				nodes, err := repo.ListEgressNodes(ctx, repository.SortQuery{})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("stored nodes=%d", len(nodes))
				for _, node := range nodes {
					if node.SourceID == source.ID {
						t.Errorf("retired fetch installed source node: %+v", node)
						_ = repo.DeleteEgressNode(ctx, node.ID)
					}
				}
				if change != "deleted" {
					after, err := repo.GetEgressSource(ctx, source.ID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, after) {
						t.Errorf("retired fetch overwrote source result: before=%+v after=%+v", before, after)
					}
				}
			})
		}
	}
}

func TestSourceSyncOlderCompletionPreservesNewerResult(t *testing.T) {
	ctx, service, repo := newFinalReviewHealthFixture(t)
	defer service.Close(ctx)
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ready, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	defer release()
	var mu sync.Mutex
	requests := 0
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		sequence := requests
		mu.Unlock()
		if sequence == 1 {
			close(ready)
			select {
			case <-released:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "http://older-feed.example:8080\n")
			return
		}
		_, _ = io.WriteString(w, "http://newer-feed.example:8080\n")
	}))
	defer feed.Close()
	publicURL := "http://1.1.1.1/overlap"
	proxyURL := feed.URL
	source, err := service.CreateSource(ctx, SubscriptionSourceInput{Name: t.Name(), Enabled: true, URL: &publicURL, ProxyURL: &proxyURL})
	if err != nil {
		t.Fatal(err)
	}
	defer service.DeleteSource(ctx, source.ID)
	done := make(chan error, 1)
	go func() { _, err := service.SyncSource(bounded, source.ID); done <- err }()
	select {
	case <-ready:
	case <-bounded.Done():
		t.Fatal(bounded.Err())
	}
	peer := NewService(repo, service.cipher)
	peer.SetSubscriptionFetcher(netfetch.NewEgressSubscriptionFetcher(nil, NormalizeSubscriptionURL))
	peer.SetSubscriptionFetcher(netfetch.NewEgressSubscriptionFetcher(nil, NormalizeSubscriptionURL))
	defer peer.Close(ctx)
	if _, err := peer.SyncSource(bounded, source.ID); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetEgressSource(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err == nil {
		t.Error("older completion was accepted after newer result")
	}
	after, err := repo.GetEgressSource(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("older completion overwrote newer metadata: before=%+v after=%+v", before, after)
	}
	nodes, err := repo.ListEgressNodesFromSource(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	enabled := 0
	for _, node := range nodes {
		if node.Enabled {
			enabled++
			plain, err := service.cipher.Decrypt(node.EncryptedProxyURL)
			if err != nil || plain != "http://newer-feed.example:8080" {
				t.Errorf("older source restored scheduling: %q %v", plain, err)
			}
		}
	}
	if enabled != 1 {
		t.Errorf("enabled source nodes=%d, want latest one", enabled)
	}
}
