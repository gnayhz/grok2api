package accountsync_test

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestInitialSyncCancellation(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, cancelOwner := range []bool{false, true} {
			name := "waiter"
			if cancelOwner {
				name = "owner"
			}
			t.Run(dialect+"/"+name, func(t *testing.T) {
				entered, enter := initialGate()
				release, unblock := initialGate()
				f := newInitialFixture(t, dialect, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/billing" {
						enter()
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					_, _ = io.WriteString(w, "ok")
				})
				t.Cleanup(unblock)
				id := f.account(t, "cancel")
				ownerCtx, ownerCancel := context.WithCancel(context.Background())
				t.Cleanup(ownerCancel)
				waiterCtx, waiterCancel := context.WithCancel(context.Background())
				t.Cleanup(waiterCancel)
				owner := startInitial(f.service, ownerCtx, id)
				awaitInitial(t, entered)
				waiter := startInitial(f.service, waiterCtx, id)
				waitInitial(t, func() bool { return f.pool.Snapshot().Active == 2 })
				time.Sleep(20 * time.Millisecond)
				if cancelOwner {
					ownerCancel()
					assertInitial(t, awaitInitial(t, owner), 0, 1)
					unblock()
					assertInitial(t, awaitInitial(t, waiter), 1, 0)
					if f.billingCalls.Load() != 2 {
						t.Fatalf("billing attempts=%d", f.billingCalls.Load())
					}
				} else {
					waiterCancel()
					select {
					case result := <-waiter:
						assertInitial(t, result, 0, 1)
					case <-time.After(250 * time.Millisecond):
						unblock()
						awaitInitial(t, owner)
						awaitInitial(t, waiter)
						t.Fatal("canceled waiter remained blocked on unrelated owner HTTP")
					}
					if f.pool.Snapshot().Active != 1 {
						t.Fatalf("owner slot lost: %+v", f.pool.Snapshot())
					}
					unblock()
					assertInitial(t, awaitInitial(t, owner), 1, 0)
					if f.billingCalls.Load() != 1 {
						t.Fatalf("billing attempts=%d", f.billingCalls.Load())
					}
				}
				if f.modelCalls.Load() != 1 {
					t.Fatalf("model observations=%d", f.modelCalls.Load())
				}
				f.assertSnapshot(t, id, true, true)
				if state := f.pool.Snapshot(); state.Active != 0 || state.Queued != 0 {
					t.Fatalf("pool not drained: %+v", state)
				}
			})
		}
	}
}

func TestInitialSyncSharesCompletionAndResumesMissingSnapshot(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, failModels := range []bool{false, true} {
			var modelFailure atomic.Bool
			modelFailure.Store(failModels)
			name := "success"
			if failModels {
				name = "model_failure"
			}
			t.Run(dialect+"/"+name, func(t *testing.T) {
				entered, enter := initialGate()
				release, unblock := initialGate()
				f := newInitialFixture(t, dialect, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/models" {
						enter()
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
						if modelFailure.Load() {
							w.WriteHeader(503)
							return
						}
					}
					_, _ = io.WriteString(w, "ok")
				})
				t.Cleanup(unblock)
				id := f.account(t, "shared")
				owner := startInitial(f.service, context.Background(), id)
				awaitInitial(t, entered)
				waiter := startInitial(f.service, context.Background(), id, id, 0)
				waitInitial(t, func() bool { return f.pool.Snapshot().Active == 2 })
				time.Sleep(20 * time.Millisecond)
				unblock()
				if failModels {
					assertInitial(t, awaitInitial(t, owner), 0, 1)
					assertInitial(t, awaitInitial(t, waiter), 0, 1)
				} else {
					assertInitial(t, awaitInitial(t, owner), 1, 0)
					assertInitial(t, awaitInitial(t, waiter), 1, 0)
				}
				f.assertSnapshot(t, id, true, !failModels)
				if f.billingCalls.Load() != 1 || f.modelCalls.Load() != 1 {
					t.Fatalf("shared calls billing=%d models=%d", f.billingCalls.Load(), f.modelCalls.Load())
				}
				// Both callers have joined before changing the HTTP fixture's failure mode.
				modelFailure.Store(false)
				assertInitial(t, f.service.Sync(context.Background(), id), 1, 0)
				expectedModels := int64(1)
				if name == "model_failure" {
					expectedModels = 2
				}
				if f.billingCalls.Load() != 1 || f.modelCalls.Load() != expectedModels {
					t.Fatalf("resume calls billing=%d models=%d", f.billingCalls.Load(), f.modelCalls.Load())
				}
				f.assertSnapshot(t, id, true, true)
			})
		}
	}
}

func TestInitialSyncPoolCancellationDrainsAndNewCallCanResume(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			entered, enter := initialGate()
			release, unblock := initialGate()
			f := newInitialFixture(t, dialect, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/billing" {
					enter()
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				_, _ = io.WriteString(w, "ok")
			})
			t.Cleanup(unblock)
			f.pool.UpdateLimit(1)
			first, second := f.account(t, "first"), f.account(t, "second")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			owner := startInitial(f.service, ctx, first)
			awaitInitial(t, entered)
			queued := startInitial(f.service, ctx, second)
			waitInitial(t, func() bool { return f.pool.Snapshot().Queued == 1 })
			cancel()
			assertInitial(t, awaitInitial(t, owner), 0, 1)
			assertInitial(t, awaitInitial(t, queued), 0, 1)
			if f.billingCalls.Load() != 1 || f.modelCalls.Load() != 0 {
				t.Fatalf("canceled calls billing=%d models=%d", f.billingCalls.Load(), f.modelCalls.Load())
			}
			if state := f.pool.Snapshot(); state.Active != 0 || state.Queued != 0 {
				t.Fatalf("pool not drained: %+v", state)
			}
			unblock()
			assertInitial(t, f.service.Sync(context.Background(), first, second), 2, 0)
			f.assertSnapshot(t, first, true, true)
			f.assertSnapshot(t, second, true, true)
		})
	}
}
