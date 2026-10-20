package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"codex-429-autoban/cpasdk/pluginapi"
)

func resetTestState() {
	banStore.clearAll()
	fallbackWRR.mu.Lock()
	fallbackWRR.currentWeight = make(map[string]int64)
	fallbackWRR.mu.Unlock()
}

func callSchedulerPick(t *testing.T, req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	respBytes, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatalf("handleSchedulerPick error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(respBytes, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope error: %v", env.Error)
	}
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
	return resp
}

func TestHandleSchedulerPick_AllHealthy(t *testing.T) {
	resetTestState()

	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "o3",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-1", Provider: providerCodex, Priority: 0},
			{ID: "auth-2", Provider: providerCodex, Priority: 0},
			{ID: "auth-3", Provider: providerCodex, Priority: 0},
		},
	}

	resp := callSchedulerPick(t, req)
	if resp.Handled {
		t.Errorf("expected Handled: false when all candidates are healthy, got Handled: true")
	}
	if resp.DelegateBuiltin != "" {
		t.Errorf("expected empty DelegateBuiltin, got %q", resp.DelegateBuiltin)
	}
	if resp.AuthID != "" {
		t.Errorf("expected empty AuthID, got %q", resp.AuthID)
	}
}

func TestHandleSchedulerPick_AllBanned(t *testing.T) {
	resetTestState()

	now := time.Now()
	banStore.set("auth-1", banEntry{ResetAt: now.Add(2 * time.Hour), Window: "5h", BannedAt: now})
	banStore.set("auth-2", banEntry{ResetAt: now.Add(2 * time.Hour), Window: "5h", BannedAt: now})

	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "o3",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-1", Provider: providerCodex, Priority: 0},
			{ID: "auth-2", Provider: providerCodex, Priority: 0},
		},
	}

	resp := callSchedulerPick(t, req)
	if resp.Handled {
		t.Errorf("expected Handled: false when all candidates are banned, got Handled: true")
	}
}

func TestHandleSchedulerPick_PartialBanned_SingleBestPriorityTier(t *testing.T) {
	resetTestState()

	now := time.Now()
	banStore.set("auth-3", banEntry{ResetAt: now.Add(2 * time.Hour), Window: "5h", BannedAt: now})

	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "o3",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-1", Provider: providerCodex, Priority: 10},
			{ID: "auth-2", Provider: providerCodex, Priority: 5},
			{ID: "auth-3", Provider: providerCodex, Priority: 20}, // banned
		},
	}

	resp := callSchedulerPick(t, req)
	if !resp.Handled {
		t.Fatalf("expected Handled: true when a candidate is banned, got false")
	}
	if resp.AuthID != "auth-1" {
		t.Errorf("expected AuthID 'auth-1' (highest remaining priority tier), got %q", resp.AuthID)
	}
}

func TestHandleSchedulerPick_PartialBanned_SessionAffinity(t *testing.T) {
	resetTestState()

	now := time.Now()
	banStore.set("auth-1", banEntry{ResetAt: now.Add(2 * time.Hour), Window: "5h", BannedAt: now})

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "auth-1", Provider: providerCodex, Priority: 0}, // banned
		{ID: "auth-2", Provider: providerCodex, Priority: 0},
		{ID: "auth-3", Provider: providerCodex, Priority: 0},
		{ID: "auth-4", Provider: providerCodex, Priority: 0},
	}

	// 1. Header session marker test
	reqWithHeader := pluginapi.SchedulerPickRequest{
		Provider:   providerCodex,
		Model:      "o3",
		Candidates: candidates,
		Options: pluginapi.SchedulerOptions{
			Headers: map[string][]string{
				"Session-Id": {"sticky-session-abc"},
			},
		},
	}

	var firstAuthID string
	for i := 0; i < 20; i++ {
		resp := callSchedulerPick(t, reqWithHeader)
		if !resp.Handled {
			t.Fatalf("iteration %d: expected Handled: true", i)
		}
		if resp.AuthID == "auth-1" {
			t.Fatalf("iteration %d: banned auth-1 was selected", i)
		}
		if i == 0 {
			firstAuthID = resp.AuthID
		} else if resp.AuthID != firstAuthID {
			t.Fatalf("iteration %d: session affinity broken: got %s, wanted %s", i, resp.AuthID, firstAuthID)
		}
	}

	// 2. Metadata session marker test
	reqWithMeta := pluginapi.SchedulerPickRequest{
		Provider:   providerCodex,
		Model:      "o3",
		Candidates: candidates,
		Options: pluginapi.SchedulerOptions{
			Metadata: map[string]any{
				"session_id": "another-session-xyz",
			},
		},
	}

	var metaAuthID string
	for i := 0; i < 20; i++ {
		resp := callSchedulerPick(t, reqWithMeta)
		if !resp.Handled {
			t.Fatalf("metadata iteration %d: expected Handled: true", i)
		}
		if resp.AuthID == "auth-1" {
			t.Fatalf("metadata iteration %d: banned auth-1 was selected", i)
		}
		if i == 0 {
			metaAuthID = resp.AuthID
		} else if resp.AuthID != metaAuthID {
			t.Fatalf("metadata iteration %d: session affinity broken: got %s, wanted %s", i, resp.AuthID, metaAuthID)
		}
	}
}

func TestHandleSchedulerPick_PartialBanned_SmoothRoundRobin(t *testing.T) {
	resetTestState()

	now := time.Now()
	banStore.set("auth-1", banEntry{ResetAt: now.Add(2 * time.Hour), Window: "5h", BannedAt: now})

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "auth-1", Provider: providerCodex, Priority: 0}, // banned
		{ID: "auth-2", Provider: providerCodex, Priority: 0},
		{ID: "auth-3", Provider: providerCodex, Priority: 0},
	}

	req := pluginapi.SchedulerPickRequest{
		Provider:   providerCodex,
		Model:      "o3",
		Candidates: candidates,
	}

	counts := make(map[string]int)
	totalRequests := 10
	for i := 0; i < totalRequests; i++ {
		resp := callSchedulerPick(t, req)
		if !resp.Handled {
			t.Fatalf("request %d: expected Handled: true", i)
		}
		if resp.AuthID == "auth-1" {
			t.Fatalf("request %d: banned auth-1 was selected", i)
		}
		counts[resp.AuthID]++
	}

	if counts["auth-2"] == 0 || counts["auth-3"] == 0 {
		t.Fatalf("expected both auth-2 and auth-3 to receive traffic, got counts: %v", counts)
	}
	if counts["auth-2"]+counts["auth-3"] != totalRequests {
		t.Fatalf("expected total %d picks, got %v", totalRequests, counts)
	}
	// With 10 requests and 2 candidates with equal weights, each should get 5
	if counts["auth-2"] != 5 || counts["auth-3"] != 5 {
		t.Errorf("expected 5 picks each for equal weights, got %v", counts)
	}
}

func TestHandleSchedulerPick_PartialBanned_WeightedSmoothRoundRobin(t *testing.T) {
	resetTestState()

	now := time.Now()
	banStore.set("auth-1", banEntry{ResetAt: now.Add(2 * time.Hour), Window: "5h", BannedAt: now})

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "auth-1", Provider: providerCodex, Priority: 0}, // banned
		{ID: "auth-2", Provider: providerCodex, Priority: 0, Attributes: map[string]string{"weight": "3"}},
		{ID: "auth-3", Provider: providerCodex, Priority: 0, Attributes: map[string]string{"weight": "1"}},
	}

	req := pluginapi.SchedulerPickRequest{
		Provider:   providerCodex,
		Model:      "o3",
		Candidates: candidates,
	}

	counts := make(map[string]int)
	totalRequests := 12
	for i := 0; i < totalRequests; i++ {
		resp := callSchedulerPick(t, req)
		if !resp.Handled {
			t.Fatalf("request %d: expected Handled: true", i)
		}
		counts[resp.AuthID]++
	}

	// 3:1 ratio across 12 requests: auth-2 should have 9, auth-3 should have 3
	if counts["auth-2"] != 9 || counts["auth-3"] != 3 {
		t.Errorf("expected 9 picks for auth-2 and 3 picks for auth-3 (3:1 ratio), got: %v", counts)
	}
}

func TestHandleSchedulerPick_ExpiredAutoReEnabled(t *testing.T) {
	resetTestState()

	// auth-1 was banned 1 hour ago, expired 10 minutes ago
	now := time.Now()
	banStore.set("auth-1", banEntry{
		ResetAt:  now.Add(-10 * time.Minute),
		Window:   "5h",
		BannedAt: now.Add(-1 * time.Hour),
	})

	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "o3",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-1", Provider: providerCodex, Priority: 0},
			{ID: "auth-2", Provider: providerCodex, Priority: 0},
		},
	}

	resp := callSchedulerPick(t, req)
	// Because auth-1 expired, all candidates are available now!
	// Therefore it should return Handled: false
	if resp.Handled {
		t.Errorf("expected Handled: false after expired credential re-enabled, got true")
	}
}

func TestHandleSchedulerPick_NonCodexIgnored(t *testing.T) {
	resetTestState()

	req := pluginapi.SchedulerPickRequest{
		Provider: "anthropic",
		Model:    "claude-3-7-sonnet",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "claude-1", Provider: "anthropic", Priority: 0},
			{ID: "claude-2", Provider: "anthropic", Priority: 0},
		},
	}

	resp := callSchedulerPick(t, req)
	if resp.Handled {
		t.Errorf("expected Handled: false for non-codex provider, got true")
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name     string
		options  pluginapi.SchedulerOptions
		expected string
	}{
		{
			name:     "empty",
			options:  pluginapi.SchedulerOptions{},
			expected: "",
		},
		{
			name: "header session-id",
			options: pluginapi.SchedulerOptions{
				Headers: http.Header{
					"Session-Id": []string{"test-session-1"},
				},
			},
			expected: "test-session-1",
		},
		{
			name: "header x-session-affinity case insensitive",
			options: pluginapi.SchedulerOptions{
				Headers: http.Header{
					"X-SESSION-AFFINITY": []string{"test-affinity-2"},
				},
			},
			expected: "test-affinity-2",
		},
		{
			name: "metadata session_id",
			options: pluginapi.SchedulerOptions{
				Metadata: map[string]any{
					"session_id": "test-meta-3",
				},
			},
			expected: "test-meta-3",
		},
		{
			name: "metadata execution_session_id",
			options: pluginapi.SchedulerOptions{
				Metadata: map[string]any{
					"execution_session_id": "test-exec-4",
				},
			},
			expected: "test-exec-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSessionID(tt.options)
			if got != tt.expected {
				t.Errorf("extractSessionID() = %q, want %q", got, tt.expected)
			}
		})
	}
}
