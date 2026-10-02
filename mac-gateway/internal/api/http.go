// Package api exposes the gateway's HTTP JSON API (debugging, scripts,
// and the phone app's non-SIP fallback).
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/gateway"
)

// NewHandler builds the HTTP API. token=="" disables auth (dev only);
// otherwise every request needs `Authorization: Bearer <token>`.
func NewHandler(ctrl *gateway.Controller, token string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ctrl.Status(r.Context()))
	})
	mux.HandleFunc("GET /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"messages": ctrl.SMS().List()})
	})
	// Voice route (module-side VoLTE provisioning) — the same code path a call
	// takes, exposed so it can be driven and diagnosed without a phone call.
	mux.HandleFunc("GET /api/v1/voice", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ctrl.VoiceStatus())
	})
	mux.HandleFunc("GET /api/v1/voice/check", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		out, err := ctrl.VoiceCheck(ctx)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/voice/start", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		out, err := ctrl.VoiceStart(ctx)
		if err != nil {
			if out == nil {
				out = map[string]any{}
			}
			out["error"] = err.Error()
			writeJSON(w, 500, out)
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/voice/stop", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := ctrl.VoiceStop(ctx); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "stopped"})
	})
	mux.HandleFunc("POST /api/v1/voice/recover", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		if err := ctrl.VoiceRecover(ctx); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "usb-reset"})
	})
	// Raw AT pass-through for diagnosing a silent call (the module's IMS/VoLTE
	// and USB-audio configuration is the first thing to look at).
	mux.HandleFunc("POST /api/v1/modem/at", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Cmd string `json:"cmd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !strings.HasPrefix(strings.ToUpper(req.Cmd), "AT") {
			writeJSON(w, 400, map[string]string{"error": `need {"cmd":"AT..."}`})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		out, err := ctrl.ATCommand(ctx, req.Cmd)
		resp := map[string]string{"cmd": req.Cmd, "output": out}
		if err != nil {
			resp["error"] = err.Error()
			writeJSON(w, 500, resp)
			return
		}
		writeJSON(w, 200, resp)
	})
	mux.HandleFunc("GET /api/v1/voice/diag", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		out, err := ctrl.VoiceDiag(ctx)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, out)
	})
	// Read-only module shell, for diagnosing a silent call (localhost only).
	mux.HandleFunc("POST /api/v1/voice/shell", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Cmd string `json:"cmd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Cmd) == "" {
			writeJSON(w, 400, map[string]string{"error": `need {"cmd":"..."}`})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		out, err := ctrl.ModuleShell(ctx, req.Cmd)
		resp := map[string]string{"cmd": req.Cmd, "output": out}
		if err != nil {
			resp["error"] = err.Error()
			writeJSON(w, 500, resp)
			return
		}
		writeJSON(w, 200, resp)
	})
	mux.HandleFunc("POST /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			To   string `json:"to"`
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.To == "" || req.Body == "" {
			writeJSON(w, 400, map[string]string{"error": "need {to, body}"})
			return
		}
		m, err := ctrl.SendSMS(r.Context(), req.To, req.Body)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, m)
	})

	// --- history sync (phone pulls on connect) ---
	mux.HandleFunc("GET /api/v1/history", func(w http.ResponseWriter, r *http.Request) {
		h := ctrl.History()
		afterCall := r.URL.Query().Get("after_call")
		afterSMS := r.URL.Query().Get("after_sms")
		resp := map[string]any{
			"calls": h.CallsSince(afterCall),
			"sms":   h.SMSSince(afterSMS),
		}
		writeJSON(w, 200, resp)
	})
	mux.HandleFunc("GET /api/v1/history/calls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"calls": ctrl.History().Calls()})
	})
	mux.HandleFunc("GET /api/v1/history/sms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"sms": ctrl.History().SMS()})
	})

	// Fake-modem triggers (only work when the gateway runs with --modem=fake).
	mux.HandleFunc("POST /api/v1/fake/incoming-call", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			From string `json:"from"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := ctrl.FakeIncomingCall(req.From); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ringing"})
	})
	mux.HandleFunc("POST /api/v1/fake/incoming-sms", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			From string `json:"from"`
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := ctrl.FakeIncomingSMS(req.From, req.Body); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "delivered"})
	})

	// --- audio diagnostics (silent-call triage) ---
	// The phone posts its audio-bridge state here, so "the app never started
	// its audio" is visible from the Mac instead of being a silent failure.
	mux.HandleFunc("POST /api/v1/client/audio-state", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State      string   `json:"state"`
			Interfaces []string `json:"interfaces"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if s := strings.TrimSpace(req.State); s != "" {
			log.Printf("phone audio: %s", s)
		}
		if len(req.Interfaces) > 0 {
			log.Printf("phone addresses: %s", strings.Join(req.Interfaces, ", "))
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/audio/devices", func(w http.ResponseWriter, r *http.Request) {
		out, err := ctrl.AudioDevices()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"devices": out})
	})
	mux.HandleFunc("POST /api/v1/audio/probe", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Device  string `json:"device"`
			Seconds int    `json:"seconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.Seconds+10)*time.Second)
		defer cancel()
		out, err := ctrl.AudioProbe(ctx, req.Device, req.Seconds)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, out)
	})

	if token == "" {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
