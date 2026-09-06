package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"codex-429-autoban/cpasdk/pluginapi"
)

func resetBanStore(t *testing.T) {
	t.Helper()
	banStore = newBanState(filepath.Join(t.TempDir(), "bans.json"))
}

func schedulerResponseForTest(t *testing.T, candidates []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	t.Helper()
	rawRequest, err := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: candidates})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rawResponse, err := handleSchedulerPick(rawRequest)
	if err != nil {
		t.Fatalf("handleSchedulerPick: %v", err)
	}
	var wrapped envelope
	if err := json.Unmarshal(rawResponse, &wrapped); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !wrapped.OK {
		t.Fatalf("plugin returned error: %s", wrapped.Error.Message)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(wrapped.Result, &response); err != nil {
		t.Fatalf("unmarshal scheduler response: %v", err)
	}
	return response
}

func TestSchedulerDoesNotOverrideHostWhenNothingIsBanned(t *testing.T) {
	resetBanStore(t)
	response := schedulerResponseForTest(t, []pluginapi.SchedulerAuthCandidate{
		{ID: "codex-1", Provider: providerCodex, Priority: 10},
		{ID: "codex-2", Provider: providerCodex, Priority: 10},
	})
	if response.Handled || response.DelegateBuiltin != "" || response.AuthID != "" {
		t.Fatalf("unexpected scheduling override: %+v", response)
	}
}

func TestSchedulerMirrorsFillFirstAfterBan(t *testing.T) {
	resetBanStore(t)
	banStore.set("codex-banned", banEntry{ResetAt: time.Now().Add(time.Hour), Window: "5h"})
	response := schedulerResponseForTest(t, []pluginapi.SchedulerAuthCandidate{
		{ID: "codex-banned", Provider: providerCodex, Priority: 100},
		{ID: "codex-z", Provider: providerCodex, Priority: 10},
		{ID: "codex-a", Provider: providerCodex, Priority: 10},
		{ID: "codex-low", Provider: providerCodex, Priority: 1},
	})
	if !response.Handled || response.DelegateBuiltin != "" {
		t.Fatalf("response must make a direct fallback selection: %+v", response)
	}
	if response.AuthID != "codex-a" {
		t.Fatalf("AuthID = %q, want native fill-first choice within the highest available priority tier: codex-a", response.AuthID)
	}
}

func TestSchedulerAllBannedDeclinesSelection(t *testing.T) {
	resetBanStore(t)
	banStore.set("codex-banned", banEntry{ResetAt: time.Now().Add(time.Hour), Window: "5h"})
	response := schedulerResponseForTest(t, []pluginapi.SchedulerAuthCandidate{{ID: "codex-banned", Provider: providerCodex}})
	if response.Handled || response.AuthID != "" {
		t.Fatalf("plugin-only API cannot fail closed: all-banned pool must be returned to stock CPA: %+v", response)
	}
}

func TestSchedulerKeepsNonCodexCandidatesInFillFirstPool(t *testing.T) {
	resetBanStore(t)
	banStore.set("codex-banned", banEntry{ResetAt: time.Now().Add(time.Hour), Window: "5h"})
	response := schedulerResponseForTest(t, []pluginapi.SchedulerAuthCandidate{
		{ID: "codex-banned", Provider: providerCodex, Priority: 100},
		{ID: "gemini-a", Provider: "gemini", Priority: 10},
		{ID: "codex-z", Provider: providerCodex, Priority: 10},
		{ID: "claude-a", Provider: "claude", Priority: 10},
	})
	if !response.Handled || response.AuthID != "claude-a" {
		t.Fatalf("mixed candidate fill-first choice = %+v, want handled claude-a", response)
	}
}

func TestSchedulerExpiresBanAndReturnsControlToHost(t *testing.T) {
	resetBanStore(t)
	banStore.set("codex-expired", banEntry{ResetAt: time.Now().Add(-time.Second), Window: "5h"})
	response := schedulerResponseForTest(t, []pluginapi.SchedulerAuthCandidate{{ID: "codex-expired", Provider: providerCodex}})
	if response.Handled || response.AuthID != "" {
		t.Fatalf("expired ban must not alter host scheduling: %+v", response)
	}
	if _, banned := banStore.lookup("codex-expired"); banned {
		t.Fatal("expired ban was not cleared")
	}
}

func TestBanStatePersistsAcrossReload(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state", "bans.json")
	state := newBanState(stateFile)
	entry := banEntry{ResetAt: time.Now().Add(time.Hour).Round(time.Second), Window: "5h", BannedAt: time.Now().Round(time.Second)}
	state.set("codex-persisted", entry)

	info, err := os.Stat(stateFile)
	if err != nil {
		t.Fatalf("stat persisted state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 600", info.Mode().Perm())
	}

	restored := newBanState(stateFile)
	restored.load()
	got, ok := restored.lookup("codex-persisted")
	if !ok || !got.ResetAt.Equal(entry.ResetAt) || got.Window != entry.Window {
		t.Fatalf("restored ban = %+v, found=%v; want %+v", got, ok, entry)
	}
}

func TestClearExpiredPersistsRemoval(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "bans.json")
	state := newBanState(stateFile)
	state.set("codex-expired", banEntry{ResetAt: time.Now().Add(-time.Second), Window: "5h"})
	if got := state.clearExpired(time.Now()); got != 1 {
		t.Fatalf("clearExpired removed %d bans, want 1", got)
	}

	restored := newBanState(stateFile)
	restored.load()
	if _, found := restored.lookup("codex-expired"); found {
		t.Fatal("expired ban remained after persisted reload")
	}
}

func TestSetNeverShortensExistingBan(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "bans.json")
	state := newBanState(stateFile)
	weekly := banEntry{ResetAt: time.Now().Add(7 * 24 * time.Hour).Round(time.Second), Window: "week"}
	state.set("codex-1", weekly)
	state.set("codex-1", banEntry{ResetAt: time.Now().Add(5 * time.Hour), Window: "5h (fallback, headers missing)"})

	restored := newBanState(stateFile)
	restored.load()
	got, found := restored.lookup("codex-1")
	if !found || !got.ResetAt.Equal(weekly.ResetAt) || got.Window != weekly.Window {
		t.Fatalf("ban was shortened: %+v, found=%v; want %+v", got, found, weekly)
	}
}

func TestConcurrentPersistsDoNotLoseBans(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "bans.json")
	state := newBanState(stateFile)
	const count = 32
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			state.set("codex-"+strconv.Itoa(i), banEntry{ResetAt: time.Now().Add(time.Hour), Window: "5h"})
		}(i)
	}
	wg.Wait()

	restored := newBanState(stateFile)
	restored.load()
	if got := len(restored.snapshot()); got != count {
		t.Fatalf("restored %d bans, want %d", got, count)
	}
}

func TestPollerLifecycleCanRestart(t *testing.T) {
	state := newBanState(filepath.Join(t.TempDir(), "bans.json"))
	state.start()
	state.stop()
	state.start()
	state.stop()
}

func TestPollerRemovesExpiredBan(t *testing.T) {
	oldInterval := cleanupPollInterval
	cleanupPollInterval = 5 * time.Millisecond
	defer func() { cleanupPollInterval = oldInterval }()

	state := newBanState(filepath.Join(t.TempDir(), "bans.json"))
	state.start()
	defer state.stop()
	state.set("codex-expired", banEntry{ResetAt: time.Now().Add(-time.Second), Window: "5h"})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, found := state.lookup("codex-expired"); !found {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("poller did not remove the expired ban")
}
