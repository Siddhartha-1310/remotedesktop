// server/main.go
// ═══════════════════════════════════════════════════════════════
//  LAPTOP C — Signaling Server
//  Run this on the laptop that acts as the server/relay.
//
//  What it does:
//    1. Manages "sessions" — each session is one controller + one target
//    2. Relays WebRTC SDP offers/answers and ICE candidates between them
//    3. Serves the browser UI for Laptop A (the controller)
//    4. Shows a dashboard at /dashboard to see all active sessions
//
//  Run:
//    go run ./server
//    go run ./server -addr :9090   (custom port)
//
//  Then on Laptop A: open http://LAPTOP_C_IP:8080
//  Then on Laptop B: go run ./agent -server ws://LAPTOP_C_IP:8080/ws
// ═══════════════════════════════════════════════════════════════

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ── Message types ────────────────────────────────────────────────────────────

type Message struct {
	Type      string `json:"type"`
	Payload   string `json:"payload,omitempty"`
	Role      string `json:"role,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// ── Session ──────────────────────────────────────────────────────────────────

// Session represents one controller ↔ target pair.
// Multiple sessions can exist simultaneously (e.g., controlling multiple machines).
type Session struct {
	ID         string
	controller *websocket.Conn
	target     *websocket.Conn
	mu         sync.Mutex
	createdAt  time.Time
}

func (s *Session) relay(from string, msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var dest *websocket.Conn
	if from == "controller" {
		dest = s.target
	} else {
		dest = s.controller
	}

	if dest == nil {
		log.Printf("[server] session=%s: no peer for %s to relay %q", s.ID, from, msg.Type)
		return
	}

	if err := dest.WriteJSON(msg); err != nil {
		log.Printf("[server] session=%s: relay to %s peer error: %v", s.ID, from, err)
	}
}

func (s *Session) bothConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controller != nil && s.target != nil
}

// ── Hub ──────────────────────────────────────────────────────────────────────

type Hub struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

func newHub() *Hub {
	return &Hub{sessions: make(map[string]*Session)}
}

func (h *Hub) getOrCreate(id string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()

	if id == "" {
		id = fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	if s, ok := h.sessions[id]; ok {
		return s
	}
	s := &Session{ID: id, createdAt: time.Now()}
	h.sessions[id] = s
	log.Printf("[server] created session: %s", id)
	return s
}

func (h *Hub) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, id)
	log.Printf("[server] removed session: %s", id)
}

func (h *Hub) stats() []map[string]interface{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]interface{}
	for _, s := range h.sessions {
		s.mu.Lock()
		out = append(out, map[string]interface{}{
			"id":                  s.ID,
			"controller_online":   s.controller != nil,
			"target_online":       s.target != nil,
			"both_connected":      s.controller != nil && s.target != nil,
			"age_seconds":         int(time.Since(s.createdAt).Seconds()),
		})
		s.mu.Unlock()
	}
	return out
}

var hub = newHub()

// ── WebSocket handler ─────────────────────────────────────────────────────────

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Step 1: Read registration — who are you and what session?
	var reg Message
	if err := conn.ReadJSON(&reg); err != nil {
		log.Printf("[server] registration read error: %v", err)
		return
	}
	if reg.Type != "register" {
		log.Printf("[server] expected 'register', got %q", reg.Type)
		return
	}

	role := reg.Role
	if role != "controller" && role != "target" {
		log.Printf("[server] unknown role %q", role)
		return
	}

	// Step 2: Get or create session
	session := hub.getOrCreate(reg.SessionID)
	log.Printf("[server] %s joined session=%s", role, session.ID)

	// Step 3: Register the connection
	session.mu.Lock()
	if role == "controller" {
		if session.controller != nil {
			session.controller.Close()
		}
		session.controller = conn
		// Tell controller the session ID (in case it was auto-generated)
		conn.WriteJSON(Message{Type: "session-id", Payload: session.ID}) //nolint:errcheck
		// If target is already up, notify controller
		if session.target != nil {
			conn.WriteJSON(Message{Type: "target-connected", SessionID: session.ID}) //nolint:errcheck
		}
	} else {
		if session.target != nil {
			session.target.Close()
		}
		session.target = conn
		// If controller is waiting, notify it
		if session.controller != nil {
			session.controller.WriteJSON(Message{Type: "target-connected", SessionID: session.ID}) //nolint:errcheck
		}
	}
	session.mu.Unlock()

	// Step 4: Relay loop
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			log.Printf("[server] %s disconnected from session=%s: %v", role, session.ID, err)
			break
		}
		log.Printf("[server] session=%s %s → %q", session.ID, role, msg.Type)
		session.relay(role, msg)
	}

	// Step 5: Cleanup on disconnect
	session.mu.Lock()
	if role == "controller" && session.controller == conn {
		session.controller = nil
		// Notify target that controller left
		if session.target != nil {
			session.target.WriteJSON(Message{Type: "controller-disconnected"}) //nolint:errcheck
		}
	} else if role == "target" && session.target == conn {
		session.target = nil
		// Notify controller that target left
		if session.controller != nil {
			session.controller.WriteJSON(Message{Type: "target-disconnected"}) //nolint:errcheck
		}
	}
	session.mu.Unlock()

	// Remove empty sessions
	session.mu.Lock()
	empty := session.controller == nil && session.target == nil
	session.mu.Unlock()
	if empty {
		hub.remove(session.ID)
	}
}

// ── REST handlers ─────────────────────────────────────────────────────────────

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"sessions": hub.stats(),
	})
}

// dashboardHandler serves a simple HTML status page
func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Server Dashboard</title>
<meta http-equiv="refresh" content="3">
<style>
body{font-family:system-ui;background:#0f0f0f;color:#e0e0e0;padding:24px}
h1{font-size:18px;margin-bottom:16px}
table{border-collapse:collapse;width:100%;font-size:13px}
th{text-align:left;padding:8px 12px;background:#1a1a1a;color:#888}
td{padding:8px 12px;border-top:1px solid #222}
.dot{width:8px;height:8px;border-radius:50%;display:inline-block;margin-right:6px}
.green{background:#4ade80}.gray{background:#555}
</style></head><body>
<h1>Remote Desktop — Server Dashboard</h1>
<p style="color:#666;font-size:12px">Auto-refreshes every 3 seconds</p>
<div id="root">Loading...</div>
<script>
fetch('/health').then(r=>r.json()).then(d=>{
  const sessions = d.sessions || [];
  if(!sessions.length){document.getElementById('root').innerHTML='<p style="color:#666">No active sessions</p>';return}
  let html='<table><tr><th>Session ID</th><th>Controller</th><th>Target</th><th>Both connected</th><th>Age</th></tr>';
  sessions.forEach(s=>{
    const dot=(v)=>v?'<span class="dot green"></span>Online':'<span class="dot gray"></span>Offline';
    html+=\`<tr><td>\${s.id}</td><td>\${dot(s.controller_online)}</td><td>\${dot(s.target_online)}</td><td>\${s.both_connected?'✅ Yes':'⏳ Waiting'}</td><td>\${s.age_seconds}s</td></tr>\`;
  });
  html+='</table>';
  document.getElementById('root').innerHTML=html;
});
</script></body></html>`)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	addr := flag.String("addr", ":8080", "Listen address")
	staticDir := flag.String("static", "./static", "Static files directory")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/dashboard", dashboardHandler)
	mux.Handle("/", http.FileServer(http.Dir(*staticDir)))

	log.Printf("════════════════════════════════════════")
	log.Printf("  Remote Desktop — Signaling Server")
	log.Printf("  Listening on %s", *addr)
	log.Printf("  Dashboard: http://localhost%s/dashboard", *addr)
	log.Printf("  Laptop A (controller): open http://LAPTOP_C_IP%s", *addr)
	log.Printf("  Laptop B (target):     go run ./agent -server ws://LAPTOP_C_IP%s/ws", *addr)
	log.Printf("════════════════════════════════════════")

	log.Fatal(http.ListenAndServe(*addr, mux))
}
