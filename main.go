// Package main implements the codex-429-autoban CPA plugin.
//
// It auto-disables a Codex credential after a 429 and auto-re-enables it
// once the rate-limit window that was hit has refreshed.
//
// Two capabilities are registered:
//   - usage_plugin: observes every completed request. On a Codex 429 it reads
//     the upstream x-codex-* response headers, decides whether the 5-hour
//     window or the weekly cap was exhausted, and records the exact reset
//     time at which the credential may be used again.
//   - scheduler: on every credential pick, it drops candidates whose recorded
//     reset time has not yet passed (lazy re-enable, since CPA exposes no
//     timer hook) and delegates the actual selection to the built-in
//     round-robin scheduler.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"codex-429-autoban/cpasdk/pluginabi"
	"codex-429-autoban/cpasdk/pluginapi"
)

const (
	pluginName    = "codex-429-autoban"
	pluginVersion = "0.1.0"

	// providerCodex is the CPA provider key for OpenAI Codex (ChatGPT backend).
	providerCodex = "codex"

	// statusTooManyRequests is the HTTP 429 status code.
	statusTooManyRequests = 429

	// Codex rate-limit window sizes, in minutes, as reported by the
	// x-codex-primary-window-minutes / x-codex-secondary-window-minutes
	// response headers.
	windowMinutes5h   = 300   // 5 hours
	windowMinutesWeek = 10080 // 7 days

	// usedPercentThreshold is the "this window is the one that tripped" marker.
	// A 429 carries the window that exhausted at ~100% used.
	usedPercentThreshold = 100
)

// banStore holds, per credential, the time at which it may be used again.
// A credential is absent from the map when it is not currently banned.
// This is in-process memory; CPA plugins are long-lived and loaded once, so
// state persists across requests. It does not survive a CPA restart, which is
// acceptable because a restart also clears CPA's own cooldown state.
var banStore banState

type banState struct {
	mu   sync.Mutex
	bans map[string]banEntry // keyed by AuthID
}

type banEntry struct {
	// ResetAt is the upstream-reported time at which the exhausted window
	// refreshes. The credential is skipped until now >= ResetAt.
	ResetAt time.Time
	// Window is a human-readable label of which limit was hit ("5h" or "week").
	Window string
	// BannedAt is when the ban was recorded, for logging only.
	BannedAt time.Time
}

// lookup returns the ban entry for the given auth ID and whether one exists.
func (s *banState) lookup(authID string) (banEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.bans[authID]
	return e, ok
}

// set records a ban for the given auth ID.
func (s *banState) set(authID string, e banEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	s.bans[authID] = e
}

// clearIfExpired removes the ban for authID if its reset time has passed.
// Returns whether the credential is currently banned AFTER this check.
func (s *banState) clearIfExpired(authID string, now time.Time) (stillBanned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.bans[authID]
	if !ok {
		return false
	}
	if !now.Before(e.ResetAt) {
		// Reset time has passed: auto re-enable.
		delete(s.bans, authID)
		slog.Info("codex-429-autoban: auto re-enabled credential",
			"auth_id", authID, "window", e.Window, "reset_at", e.ResetAt.Format(time.RFC3339))
		return false
	}
	return true
}

func main() {}

// cliproxy_plugin_init is the native entry point CPA calls when loading the
// plugin. It wires the host reverse-call API and registers our call/free/shutdown
// function pointers.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// cliproxyPluginCall is the single dispatch entry CPA invokes for every method.
//
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// handleMethod routes a CPA method to its handler.
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// pluginRegistration declares the plugin's metadata and capabilities.
// Both usage_plugin and scheduler must be true.
func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "local",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: registrationCapability{
			UsagePlugin: true,
			Scheduler:   true,
		},
	}
}

// handleUsage observes a completed request. On a Codex 429 it records the
// ban; otherwise it is a no-op.
func handleUsage(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return okEnvelope(map[string]any{})
	}
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		slog.Warn("codex-429-autoban: failed to decode usage record", "error", errUnmarshal)
		return okEnvelope(map[string]any{})
	}

	// Only Codex credentials are in scope.
	if !strings.EqualFold(record.Provider, providerCodex) {
		return okEnvelope(map[string]any{})
	}
	// Only act on 429 failures.
	if !record.Failed || record.Failure.StatusCode != statusTooManyRequests {
		return okEnvelope(map[string]any{})
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		slog.Warn("codex-429-autoban: 429 received but AuthID is empty, cannot ban")
		return okEnvelope(map[string]any{})
	}

	entry, ok := classifyAndBuildBan(record.ResponseHeaders)
	if !ok {
		// Could not determine which window was hit from the headers.
		// Fall back to a conservative 5-hour ban so the credential is not
		// hammered while rate-limited, matching the more common case.
		now := time.Now()
		entry = banEntry{
			ResetAt:  now.Add(5 * time.Hour),
			Window:   "5h (fallback, headers missing)",
			BannedAt: now,
		}
		slog.Warn("codex-429-autoban: x-codex-* headers missing on 429, falling back to 5h ban",
			"auth_id", authID)
	} else {
		entry.BannedAt = time.Now()
	}

	banStore.set(authID, entry)
	slog.Info("codex-429-autoban: banned credential after 429",
		"auth_id", authID,
		"window", entry.Window,
		"reset_at", entry.ResetAt.Format(time.RFC3339))
	return okEnvelope(map[string]any{})
}

// classifyAndBuildBan inspects the upstream x-codex-* response headers and
// decides which rate-limit window was exhausted, returning the ban entry with
// the corresponding reset time. Returns ok=false when the headers are absent
// or inconclusive.
//
// Header reference (ChatGPT/Codex backend, not the public Platform API):
//   - x-codex-primary-window-minutes   = 300 for the 5-hour window
//   - x-codex-primary-reset-at         = Unix seconds, 5-hour window reset
//   - x-codex-primary-used-percent     = 0-100
//   - x-codex-secondary-window-minutes = 10080 for the weekly window
//   - x-codex-secondary-reset-at       = Unix seconds, weekly window reset
//   - x-codex-secondary-used-percent   = 0-100
func classifyAndBuildBan(headers http.Header) (banEntry, bool) {
	h := headers

	primaryUsed := headerFloat(h, "x-codex-primary-used-percent")
	secondaryUsed := headerFloat(h, "x-codex-secondary-used-percent")
	primaryReset := headerUnixTime(h, "x-codex-primary-reset-at")
	secondaryReset := headerUnixTime(h, "x-codex-secondary-reset-at")

	// Prefer the explicit "which window is full" signal: the window whose
	// used-percent reached the threshold. If both are present, pick the one
	// at threshold; if only one header family is present, use that.
	primaryFull := primaryUsed >= usedPercentThreshold
	secondaryFull := secondaryUsed >= usedPercentThreshold

	switch {
	case secondaryFull && !primaryFull:
		if !secondaryReset.IsZero() {
			return banEntry{ResetAt: secondaryReset, Window: "week"}, true
		}
	case primaryFull && !secondaryFull:
		if !primaryReset.IsZero() {
			return banEntry{ResetAt: primaryReset, Window: "5h"}, true
		}
	case primaryFull && secondaryFull:
		// Both exhausted: must wait for the later reset (weekly) to be safe.
		if !secondaryReset.IsZero() {
			return banEntry{ResetAt: secondaryReset, Window: "week (both full)"}, true
		}
		if !primaryReset.IsZero() {
			return banEntry{ResetAt: primaryReset, Window: "5h (both full, weekly reset missing)"}, true
		}
	default:
		// Neither reports as full via used-percent. Fall back to window-minutes
		// identity if a reset time is present, else give up.
		if !primaryReset.IsZero() && headerInt(h, "x-codex-primary-window-minutes") == windowMinutes5h {
			return banEntry{ResetAt: primaryReset, Window: "5h"}, true
		}
		if !secondaryReset.IsZero() && headerInt(h, "x-codex-secondary-window-minutes") == windowMinutesWeek {
			return banEntry{ResetAt: secondaryReset, Window: "week"}, true
		}
	}
	return banEntry{}, false
}

// handleSchedulerPick filters out credentials that are still banned, then
// delegates the actual selection to the built-in round-robin scheduler.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	now := time.Now()
	available := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		// Only Codex credentials are subject to our bans.
		if !strings.EqualFold(candidate.Provider, providerCodex) {
			available = append(available, candidate)
			continue
		}
		// clearIfExpired auto-re-enables credentials whose reset time passed.
		if banStore.clearIfExpired(candidate.ID, now) {
			// Still banned: drop from the candidate list.
			continue
		}
		available = append(available, candidate)
	}

	// If every Codex candidate is banned (and there were no non-Codex ones),
	// decline to handle so CPA's own logic can decide (e.g. wait on its
	// built-in cooldown, or return an error). We do not force a pick here.
	if len(available) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// CPA applies our response as follows (conductor.go):
	//   - if AuthID is set and matches a candidate  -> use exactly that one
	//   - else if DelegateBuiltin is set            -> run the built-in
	//                                                   scheduler over the FULL
	//                                                   candidate set (it cannot
	//                                                   be shrunk by the plugin)
	//   - else (Handled false)                      -> host falls back to its
	//                                                   own built-in scheduler
	//
	// Because DelegateBuiltin would let round-robin pick a banned credential,
	// when anything is banned we pick an available AuthID ourselves. When
	// nothing is banned we delegate to round-robin to preserve normal
	// load-balancing.
	if len(available) == len(req.Candidates) {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin,
			Handled:         true,
		})
	}
	// Pick the available candidate with the highest numeric priority value
	// (CPA's convention: higher priority value = higher precedence).
	chosen := available[0]
	for _, c := range available[1:] {
		if c.Priority > chosen.Priority {
			chosen = c
		}
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  chosen.ID,
		Handled: true,
	})
}

// ---- header helpers ----

func headerFloat(h http.Header, key string) float64 {
	raw := h.Get(key)
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return v
}

func headerInt(h http.Header, key string) int {
	raw := h.Get(key)
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return v
}

func headerUnixTime(h http.Header, key string) time.Time {
	raw := h.Get(key)
	if raw == "" {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// ---- envelope / response helpers ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	UsagePlugin bool `json:"usage_plugin"`
	Scheduler   bool `json:"scheduler"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
