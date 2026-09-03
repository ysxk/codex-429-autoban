package main

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyAndBuildBanRecognizesMonthlyFreeTierWindow(t *testing.T) {
	resetAt := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	headers := http.Header{
		"X-Codex-Primary-Window-Minutes": []string{"43200"},
		"X-Codex-Primary-Reset-At":       []string{formatUnix(resetAt)},
		"X-Codex-Primary-Used-Percent":   []string{"100"},
	}

	entry, ok := classifyAndBuildBan(headers)
	if !ok {
		t.Fatal("monthly quota window should be recognized")
	}
	if entry.Window != "month" {
		t.Fatalf("window = %q, want month", entry.Window)
	}
	if !entry.ResetAt.Equal(resetAt) {
		t.Fatalf("reset_at = %s, want %s", entry.ResetAt, resetAt)
	}
}

func TestClassifyAndBuildBanUsesLatestResetWhenMultipleWindowsAreFull(t *testing.T) {
	earlier := time.Now().Add(5 * time.Hour).Truncate(time.Second)
	monthly := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	headers := http.Header{
		"X-Codex-Primary-Window-Minutes":   []string{"300"},
		"X-Codex-Primary-Reset-At":         []string{formatUnix(earlier)},
		"X-Codex-Primary-Used-Percent":     []string{"100"},
		"X-Codex-Secondary-Window-Minutes": []string{"43200"},
		"X-Codex-Secondary-Reset-At":       []string{formatUnix(monthly)},
		"X-Codex-Secondary-Used-Percent":   []string{"100"},
	}

	entry, ok := classifyAndBuildBan(headers)
	if !ok {
		t.Fatal("full windows should be recognized")
	}
	if entry.Window != "month (both full)" {
		t.Fatalf("window = %q, want month (both full)", entry.Window)
	}
	if !entry.ResetAt.Equal(monthly) {
		t.Fatalf("reset_at = %s, want %s", entry.ResetAt, monthly)
	}
}

func TestClassifyAndBuildBanDoesNotTreatUnknownResetAsFiveHours(t *testing.T) {
	resetAt := time.Now().Add(45 * 24 * time.Hour).Truncate(time.Second)
	headers := http.Header{
		"X-Codex-Primary-Window-Minutes": []string{"64800"},
		"X-Codex-Primary-Reset-At":       []string{formatUnix(resetAt)},
		"X-Codex-Primary-Used-Percent":   []string{"100"},
	}

	entry, ok := classifyAndBuildBan(headers)
	if !ok {
		t.Fatal("custom quota window with reset_at should be recognized")
	}
	if entry.ResetAt.Equal(time.Now().Add(5 * time.Hour)) {
		t.Fatal("custom reset must not fall back to five hours")
	}
	if !entry.ResetAt.Equal(resetAt) {
		t.Fatalf("reset_at = %s, want %s", entry.ResetAt, resetAt)
	}
}

func TestHandleUsageUsesMonthlyFallbackWhenResetHeadersAreMissing(t *testing.T) {
	before := time.Now()
	raw := `{"Provider":"codex","Failed":true,"AuthID":"test-auth","Failure":{"StatusCode":429},"ResponseHeaders":{}}`

	if _, err := handleUsage([]byte(raw)); err != nil {
		t.Fatalf("handleUsage returned error: %v", err)
	}

	entry, ok := banStore.lookup("test-auth")
	if !ok {
		t.Fatal("429 without headers should create a ban")
	}
	if entry.Window != "month (fallback, headers missing)" {
		t.Fatalf("window = %q, want monthly fallback", entry.Window)
	}
	minReset := before.Add(30 * 24 * time.Hour)
	if entry.ResetAt.Before(minReset) {
		t.Fatalf("reset_at = %s, should be at least 30 days from start", entry.ResetAt)
	}
	banStore.clearAll()
}

func formatUnix(t time.Time) string {
	return time.Unix(t.Unix(), 0).Format("2006-01-02T15:04:05Z07:00")
}
