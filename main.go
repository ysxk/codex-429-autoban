// Package main implements the codex-429-autoban CPA plugin.
//
// It auto-disables a Codex credential after a 429 and auto-re-enables it
// once the rate-limit window that was hit has refreshed.
//
// Three capabilities are registered:
//   - usage_plugin: observes every completed request. On a Codex 429 it reads
//     the upstream x-codex-* response headers, decides whether the 5-hour
//     window or the weekly cap was exhausted, and records the exact reset
//     time at which the credential may be used again.
	//   - scheduler: on every credential pick, it drops candidates whose recorded
	//     reset time has not yet passed (lazy re-enable, since CPA exposes no
	//     timer hook). When all candidates are healthy, it returns Handled: false
	//     to preserve host routing (session affinity, weighted round-robin).
	//     When a subset is banned, it dispatches healthy candidates via session
	//     affinity hashing or smooth weighted round-robin.
//   - management_api: exposes a small status page and authenticated API for
//     manually clearing the in-memory ban state after the user resets Codex
//     quota or uses a reset card upstream.
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
	"html"
	"log/slog"
	"net/http"
	"sort"
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
	pluginVersion = "0.2.3"

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

	managementRoutePrefix = "/plugins/" + pluginName
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

// clearExpired removes every ban whose reset time has passed.
func (s *banState) clearExpired(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for authID, e := range s.bans {
		if !now.Before(e.ResetAt) {
			delete(s.bans, authID)
			removed++
			slog.Info("codex-429-autoban: auto re-enabled credential",
				"auth_id", authID, "window", e.Window, "reset_at", e.ResetAt.Format(time.RFC3339))
		}
	}
	return removed
}

// clear removes the ban for authID, if present.
func (s *banState) clear(authID string) (banEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		return banEntry{}, false
	}
	e, ok := s.bans[authID]
	if ok {
		delete(s.bans, authID)
	}
	return e, ok
}

// clearAll removes every active ban and returns how many were removed.
func (s *banState) clearAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.bans)
	s.bans = make(map[string]banEntry)
	return n
}

// snapshot returns a copy of the current bans for diagnostics / management UI.
func (s *banState) snapshot() map[string]banEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]banEntry, len(s.bans))
	for authID, e := range s.bans {
		out[authID] = e
	}
	return out
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
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
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
			Author:           "ysxk",
			GitHubRepository: "https://github.com/ysxk/codex-429-autoban",
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: registrationCapability{
			UsagePlugin:   true,
			Scheduler:     true,
			ManagementAPI: true,
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

// extractSessionID looks for explicit or derived session markers in request headers and metadata.
func extractSessionID(options pluginapi.SchedulerOptions) string {
	sessionHeaderKeys := [...]string{
		"session-id",
		"session_id",
		"x-session-id",
		"x-session-affinity",
		"x-slot-session-id",
		"x-conversation-id",
		"x-thread-id",
		"x-client-request-id",
		"x-claude-code-session-id",
	}
	if len(options.Headers) > 0 {
		for _, key := range sessionHeaderKeys {
			for hKey, values := range options.Headers {
				if strings.EqualFold(hKey, key) && len(values) > 0 {
					val := strings.TrimSpace(values[0])
					if val != "" {
						return val
					}
				}
			}
		}
	}

	if len(options.Metadata) > 0 {
		metadataKeys := [...]string{
			"session_id",
			"execution_session_id",
			"derived_session_id",
			"parent_session_id",
			"conversation_id",
		}
		for _, key := range metadataKeys {
			if raw, ok := options.Metadata[key]; ok && raw != nil {
				if str, okStr := raw.(string); okStr {
					val := strings.TrimSpace(str)
					if val != "" {
						return val
					}
				}
			}
		}
	}
	return ""
}

// candidateWeight extracts the positive integer weight of an auth candidate, defaulting to 1.
func candidateWeight(c pluginapi.SchedulerAuthCandidate) int64 {
	if c.Attributes != nil {
		if raw, ok := c.Attributes["weight"]; ok {
			if w, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && w > 0 {
				return w
			}
		}
	}
	if c.Metadata != nil {
		if raw, ok := c.Metadata["weight"]; ok && raw != nil {
			switch v := raw.(type) {
			case int:
				if v > 0 {
					return int64(v)
				}
			case int64:
				if v > 0 {
					return v
				}
			case float64:
				if v > 0 {
					return int64(v)
				}
			case string:
				if w, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && w > 0 {
					return w
				}
			}
		}
	}
	return 1
}

// fnv1a64 computes a 64-bit FNV-1a hash of s.
func fnv1a64(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// pickBySessionAffinity deterministically maps a sessionID to a candidate in candidates,
// honoring relative candidate weights to maximize prompt cache reuse.
func pickBySessionAffinity(candidates []pluginapi.SchedulerAuthCandidate, sessionID string) pluginapi.SchedulerAuthCandidate {
	if len(candidates) == 0 {
		return pluginapi.SchedulerAuthCandidate{}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	sorted := make([]pluginapi.SchedulerAuthCandidate, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})

	var totalWeight int64
	for _, c := range sorted {
		totalWeight += candidateWeight(c)
	}
	if totalWeight <= 0 {
		totalWeight = int64(len(sorted))
	}

	target := int64(fnv1a64(sessionID) % uint64(totalWeight))
	var cumulative int64
	for _, c := range sorted {
		cumulative += candidateWeight(c)
		if target < cumulative {
			return c
		}
	}
	return sorted[len(sorted)-1]
}

// weightedRoundRobinPicker implements NGINX-style smooth weighted round-robin.
type weightedRoundRobinPicker struct {
	mu            sync.Mutex
	currentWeight map[string]int64
}

var fallbackWRR weightedRoundRobinPicker

func (p *weightedRoundRobinPicker) pick(candidates []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerAuthCandidate {
	if len(candidates) == 0 {
		return pluginapi.SchedulerAuthCandidate{}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.currentWeight == nil {
		p.currentWeight = make(map[string]int64)
	} else if len(p.currentWeight) > 1024 {
		valid := make(map[string]bool, len(candidates))
		for _, c := range candidates {
			valid[c.ID] = true
		}
		for k := range p.currentWeight {
			if !valid[k] {
				delete(p.currentWeight, k)
			}
		}
	}

	var totalWeight int64
	var bestCandidate pluginapi.SchedulerAuthCandidate
	var maxCurrent int64 = -1 << 62
	foundBest := false

	for _, c := range candidates {
		w := candidateWeight(c)
		totalWeight += w
		p.currentWeight[c.ID] += w
		if !foundBest || p.currentWeight[c.ID] > maxCurrent {
			maxCurrent = p.currentWeight[c.ID]
			bestCandidate = c
			foundBest = true
		}
	}

	p.currentWeight[bestCandidate.ID] -= totalWeight
	return bestCandidate
}

// handleSchedulerPick filters out credentials that are still banned.
//
// CPA integration rules:
//  1. When no candidates are banned (len(available) == len(Candidates)), we return Handled: false.
//     This lets CPA's own router/selector execute without interference, preserving user-configured
//     routing strategies such as session affinity (prompt cache reuse) and weighted round-robin.
//  2. When every candidate is banned (len(available) == 0), we return Handled: false so CPA can
//     trigger its native cooling / failover / error handling.
//  3. When a subset of candidates is banned, CPA's DelegateBuiltin cannot restrict its candidate pool
//     to the healthy subset (it always operates over the full candidate set). Therefore, the plugin
//     must pick a healthy AuthID directly:
//       - Candidates are grouped by highest priority tier.
//       - If a session marker (header/metadata) is present, deterministic weighted affinity hashing
//         is used to stick the session to a healthy credential.
//       - If no session marker is present, smooth weighted round-robin distributes traffic evenly.
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

	// Case 1: Every candidate is banned (or candidate list was empty).
	// Decline to handle so CPA can decide (e.g. wait on its built-in cooldown, or return an error).
	if len(available) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Case 2: No candidates are banned.
	// Return Handled: false to let CPA execute user-configured selector strategies (session affinity,
	// weighted round-robin, fill-first) seamlessly.
	if len(available) == len(req.Candidates) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Case 3: A partial set of candidates is banned. Pick an available candidate directly.
	// Step 3a: Select highest priority tier.
	maxPriority := available[0].Priority
	for _, c := range available[1:] {
		if c.Priority > maxPriority {
			maxPriority = c.Priority
		}
	}
	var bestTier []pluginapi.SchedulerAuthCandidate
	for _, c := range available {
		if c.Priority == maxPriority {
			bestTier = append(bestTier, c)
		}
	}

	if len(bestTier) == 1 {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			AuthID:  bestTier[0].ID,
			Handled: true,
		})
	}

	// Step 3b: Sticky session affinity if request carries a session marker.
	if sessionID := extractSessionID(req.Options); sessionID != "" {
		chosen := pickBySessionAffinity(bestTier, sessionID)
		return okEnvelope(pluginapi.SchedulerPickResponse{
			AuthID:  chosen.ID,
			Handled: true,
		})
	}

	// Step 3c: Fall back to smooth weighted round-robin among the healthy best tier.
	chosen := fallbackWRR.pick(bestTier)
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  chosen.ID,
		Handled: true,
	})
}

// managementRegistration exposes a small Management API and resource page so
// users can put an auth back into the pool after manually resetting Codex
// quota or using a reset card. CPA does not provide a timer/event for that
// upstream-side action, so manual unban is the reliable integration point.
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodGet,
				Path:        managementRoutePrefix + "/bans",
				Description: "List Codex auths currently held out of the pool by codex-429-autoban.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/unban",
				Description: "Remove one Codex auth from the in-memory ban list. Body: {\"auth_id\":\"...\"}.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/unban-all",
				Description: "Remove every Codex auth from the in-memory ban list.",
			},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        "Codex 429 Autoban",
				Description: "View and manually unban Codex credentials after a quota reset.",
			},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return okEnvelope(dispatchManagement(req))
}

func dispatchManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case method == http.MethodGet && matchesManagementPath(req.Path, "/bans"):
		return jsonManagementResponse(http.StatusOK, currentBanStatus())
	case method == http.MethodPost && matchesManagementPath(req.Path, "/unban"):
		return handleManagementUnban(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/unban-all"):
		return handleManagementUnbanAll()
	case method == http.MethodGet && matchesResourcePath(req.Path, "/status"):
		return htmlManagementResponse(http.StatusOK, managementStatusPage())
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]any{
			"error":  "not_found",
			"path":   req.Path,
			"method": method,
		})
	}
}

type managementBanStatus struct {
	Plugin  string              `json:"plugin"`
	Version string              `json:"version"`
	Count   int                 `json:"count"`
	Bans    []managementBanInfo `json:"bans"`
}

type managementBanInfo struct {
	AuthID           string `json:"auth_id"`
	Window           string `json:"window"`
	BannedAt         string `json:"banned_at,omitempty"`
	BannedAtUnix     int64  `json:"banned_at_unix,omitempty"`
	ResetAt          string `json:"reset_at"`
	ResetAtUnix      int64  `json:"reset_at_unix"`
	RemainingSeconds int64  `json:"remaining_seconds"`
}

func currentBanStatus() managementBanStatus {
	now := time.Now()
	banStore.clearExpired(now)
	snapshot := banStore.snapshot()
	bans := make([]managementBanInfo, 0, len(snapshot))
	for authID, entry := range snapshot {
		remaining := int64(0)
		if now.Before(entry.ResetAt) {
			remaining = int64(entry.ResetAt.Sub(now).Seconds())
		}
		info := managementBanInfo{
			AuthID:           authID,
			Window:           entry.Window,
			ResetAt:          entry.ResetAt.Format(time.RFC3339),
			ResetAtUnix:      entry.ResetAt.Unix(),
			RemainingSeconds: remaining,
		}
		if !entry.BannedAt.IsZero() {
			info.BannedAt = entry.BannedAt.Format(time.RFC3339)
			info.BannedAtUnix = entry.BannedAt.Unix()
		}
		bans = append(bans, info)
	}
	sort.Slice(bans, func(i, j int) bool {
		if bans[i].ResetAtUnix == bans[j].ResetAtUnix {
			return bans[i].AuthID < bans[j].AuthID
		}
		return bans[i].ResetAtUnix < bans[j].ResetAtUnix
	})
	return managementBanStatus{
		Plugin:  pluginName,
		Version: pluginVersion,
		Count:   len(bans),
		Bans:    bans,
	}
}

type managementUnbanRequest struct {
	AuthID string `json:"auth_id"`
	All    bool   `json:"all"`
}

func handleManagementUnban(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body managementUnbanRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{
				"error":   "invalid_json",
				"message": errUnmarshal.Error(),
			})
		}
	}
	if strings.EqualFold(req.Query.Get("all"), "true") || body.All {
		return handleManagementUnbanAll()
	}

	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(req.Query.Get("auth_id"))
	}
	if authID == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error":   "missing_auth_id",
			"message": "provide auth_id in JSON body or query string",
		})
	}

	entry, removed := banStore.clear(authID)
	if removed {
		slog.Info("codex-429-autoban: manually re-enabled credential",
			"auth_id", authID, "window", entry.Window, "reset_at", entry.ResetAt.Format(time.RFC3339))
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"auth_id": authID,
		"removed": removed,
		"status":  currentBanStatus(),
	})
}

func handleManagementUnbanAll() pluginapi.ManagementResponse {
	removed := banStore.clearAll()
	if removed > 0 {
		slog.Info("codex-429-autoban: manually re-enabled all credentials", "removed", removed)
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"removed": removed,
		"status":  currentBanStatus(),
	})
}

func matchesManagementPath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return strings.HasSuffix(path, managementRoutePrefix+suffix)
}

func matchesResourcePath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return strings.HasSuffix(path, "/v0/resource/plugins/"+pluginName+suffix) ||
		strings.HasSuffix(path, "/plugins/"+pluginName+suffix)
}

func jsonManagementResponse(status int, v any) pluginapi.ManagementResponse {
	raw, errMarshal := json.MarshalIndent(v, "", "  ")
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(map[string]any{
			"error":   "marshal_error",
			"message": errMarshal.Error(),
		})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
		},
		Body: raw,
	}
}

func htmlManagementResponse(status int, body string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"text/html; charset=utf-8"},
		},
		Body: []byte(body),
	}
}

func managementStatusPage() string {
	version := html.EscapeString(pluginVersion)
	return `<!doctype html>
<html lang="zh-Hans">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>codex-429-autoban</title>
  <style>
    :root { color-scheme: light dark; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { max-width: 980px; margin: 32px auto; padding: 0 16px; line-height: 1.5; }
    h1 { margin-bottom: 4px; }
    .muted { color: #667085; }
    .card { border: 1px solid #d0d7de; border-radius: 12px; padding: 16px; margin: 16px 0; }
    label { display: block; font-weight: 600; margin-bottom: 6px; }
    input { width: min(640px, 100%); padding: 8px 10px; border: 1px solid #d0d7de; border-radius: 8px; }
    button { cursor: pointer; padding: 8px 12px; border: 1px solid #d0d7de; border-radius: 8px; margin: 4px 4px 4px 0; }
    button.primary { background: #0969da; border-color: #0969da; color: white; }
    button.danger { background: #cf222e; border-color: #cf222e; color: white; }
    table { width: 100%; border-collapse: collapse; margin-top: 12px; }
    th, td { border-bottom: 1px solid #d0d7de; padding: 8px; text-align: left; vertical-align: top; }
    code { background: rgba(127,127,127,.15); padding: 2px 4px; border-radius: 4px; }
    pre { overflow: auto; background: rgba(127,127,127,.12); padding: 12px; border-radius: 8px; }
  </style>
</head>
<body>
  <h1>codex-429-autoban</h1>
  <p class="muted">版本 ` + version + ` · 手动把已重置额度的 Codex 账号加回号池。</p>

  <div class="card">
    <p>资源页本身不带管理鉴权。要执行查看/解除操作，请填入 CPA 管理密钥；请求会使用 <code>Authorization: Bearer &lt;key&gt;</code>。</p>
    <label for="key">CPA 管理密钥</label>
    <input id="key" type="password" autocomplete="current-password" placeholder="Management key">
    <div>
      <button class="primary" onclick="refresh()">刷新当前 ban 列表</button>
      <button class="danger" onclick="unbanAll()">全部加回号池</button>
    </div>
    <p id="message" class="muted"></p>
  </div>

  <div class="card">
    <h2>当前被插件排除的账号</h2>
    <div id="list">尚未加载。</div>
  </div>

  <div class="card">
    <h2>API</h2>
    <pre>GET  /v0/management/plugins/codex-429-autoban/bans
POST /v0/management/plugins/codex-429-autoban/unban      {"auth_id":"..."}
POST /v0/management/plugins/codex-429-autoban/unban-all</pre>
  </div>

  <script>
    const apiBase = "/v0/management/plugins/codex-429-autoban";
    const keyInput = document.getElementById("key");
    const savedKey = localStorage.getItem("codex429AutobanManagementKey") || "";
    keyInput.value = savedKey;
    keyInput.addEventListener("change", function () {
      localStorage.setItem("codex429AutobanManagementKey", keyInput.value);
    });

    function headers() {
      const h = {"Content-Type": "application/json"};
      if (keyInput.value) h.Authorization = "Bearer " + keyInput.value;
      return h;
    }

    function setMessage(text, isError) {
      const el = document.getElementById("message");
      el.textContent = text || "";
      el.style.color = isError ? "#cf222e" : "";
    }

    async function call(path, options) {
      const resp = await fetch(apiBase + path, Object.assign({headers: headers()}, options || {}));
      const text = await resp.text();
      let data;
      try { data = JSON.parse(text); } catch (_) { data = {raw: text}; }
      if (!resp.ok) {
        throw new Error((data && (data.message || data.error)) || ("HTTP " + resp.status));
      }
      return data;
    }

    function formatRemaining(seconds) {
      seconds = Math.max(0, Number(seconds || 0));
      const h = Math.floor(seconds / 3600);
      const m = Math.floor((seconds % 3600) / 60);
      if (h > 0) return h + "h " + m + "m";
      return m + "m";
    }

    function render(data) {
      const list = document.getElementById("list");
      if (!data.bans || data.bans.length === 0) {
        list.innerHTML = "<p>没有账号被插件 ban，号池无需手动恢复。</p>";
        return;
      }
      let html = "<table><thead><tr><th>Auth ID</th><th>窗口</th><th>解除时间</th><th>剩余</th><th>操作</th></tr></thead><tbody>";
      for (const ban of data.bans) {
        html += "<tr><td><code>" + escapeHtml(ban.auth_id) + "</code></td><td>" + escapeHtml(ban.window) + "</td><td>" + escapeHtml(ban.reset_at) + "</td><td>" + formatRemaining(ban.remaining_seconds) + "</td><td><button onclick=\"unban('" + escapeJs(ban.auth_id) + "')\">加回号池</button></td></tr>";
      }
      html += "</tbody></table>";
      list.innerHTML = html;
    }

    function escapeHtml(value) {
      return String(value || "").replace(/[&<>"']/g, function (c) {
        return {"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c];
      });
    }

    function escapeJs(value) {
      return String(value || "").replace(/\\/g, "\\\\").replace(/'/g, "\\'");
    }

    async function refresh() {
      try {
        setMessage("加载中...");
        const data = await call("/bans");
        render(data);
        setMessage("已刷新，共 " + data.count + " 个账号被排除。");
      } catch (err) {
        setMessage(err.message, true);
      }
    }

    async function unban(authID) {
      if (!confirm("确认将 " + authID + " 加回号池？")) return;
      try {
        const data = await call("/unban", {method: "POST", body: JSON.stringify({auth_id: authID})});
        render(data.status);
        setMessage(data.removed ? "已加回号池：" + authID : "该账号当前不在 ban 列表：" + authID);
      } catch (err) {
        setMessage(err.message, true);
      }
    }

    async function unbanAll() {
      if (!confirm("确认清空全部 ban 状态？")) return;
      try {
        const data = await call("/unban-all", {method: "POST", body: "{}"});
        render(data.status);
        setMessage("已解除 " + data.removed + " 个账号。");
      } catch (err) {
        setMessage(err.message, true);
      }
    }
  </script>
</body>
</html>`
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
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
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
