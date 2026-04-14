// agent/main.go
// ═══════════════════════════════════════════════════════════════
//  LAPTOP B — Target Agent
//  Run this on the machine you want to control remotely.
//
//  Run:
//    go run ./agent -server ws://LAPTOP_C_IP:8080/ws -session default
//
//  Linux deps:
//    sudo apt install libx11-dev libxtst-dev libxinerama-dev libxrandr-dev
// ═══════════════════════════════════════════════════════════════

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-vgo/robotgo"
	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
	"github.com/pion/webrtc/v3"
)

var (
	serverURL      = flag.String("server", "ws://localhost:8080/ws", "Signaling server URL")
	sessionID      = flag.String("session", "default", "Session ID")
	captureQuality = flag.Int("quality", 40, "JPEG quality 1-100")
	captureFPS     = flag.Int("fps", 10, "Frames per second")
	displayIdx     = flag.Int("display", 0, "Display index")
	retryDelay     = flag.Duration("retry", 5*time.Second, "Reconnect delay")
)

// ── Screen Capture ────────────────────────────────────────────────────────────

func captureFrame() ([]byte, error) {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		return nil, nil
	}
	idx := *displayIdx
	if idx >= n {
		idx = 0
	}
	img, err := screenshot.CaptureRect(screenshot.GetDisplayBounds(idx))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaleDown(img, 1280), &jpeg.Options{Quality: *captureQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func scaleDown(img *image.RGBA, maxW int) image.Image {
	b := img.Bounds()
	if b.Dx() <= maxW {
		return img
	}
	ratio := float64(maxW) / float64(b.Dx())
	newH := int(float64(b.Dy()) * ratio)
	dst := image.NewRGBA(image.Rect(0, 0, maxW, newH))
	for y := 0; y < newH; y++ {
		for x := 0; x < maxW; x++ {
			dst.Set(x, y, img.At(b.Min.X+int(float64(x)/ratio), b.Min.Y+int(float64(y)/ratio)))
		}
	}
	return dst
}

// ── Input Injection ───────────────────────────────────────────────────────────

type InputEvent struct {
	Type   string  `json:"type"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button int     `json:"button"`
	Key    string  `json:"key"`
	Code   string  `json:"code"`
	DeltaY float64 `json:"deltaY"`
}

func absXY(xPct, yPct float64) (int, int, bool) {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		return 0, 0, false
	}
	idx := *displayIdx
	if idx >= n {
		idx = 0
	}
	b := screenshot.GetDisplayBounds(idx)
	clamp := func(v float64) float64 { return math.Min(100, math.Max(0, v)) }
	x := b.Min.X + int(math.Round(clamp(xPct)/100*float64(b.Dx()-1)))
	y := b.Min.Y + int(math.Round(clamp(yPct)/100*float64(b.Dy()-1)))
	return x, y, true
}

func btnName(b int) string {
	switch b {
	case 1:
		return "center"
	case 2:
		return "right"
	default:
		return "left"
	}
}

func mapKey(ev InputEvent) (string, bool) {
	if k := mapBrowserKey(ev.Key); k != "" {
		return k, true
	}
	if k := mapBrowserCode(ev.Code); k != "" {
		return k, true
	}
	return "", false
}

func mapBrowserKey(key string) string {
	if key == " " {
		return "space"
	}
	if len(key) == 1 {
		return strings.ToLower(key)
	}
	m := map[string]string{
		"enter": "enter", "tab": "tab", "escape": "escape", "esc": "escape",
		"backspace": "backspace", "delete": "delete", "insert": "insert",
		"home": "home", "end": "end", "pageup": "pageup", "pagedown": "pagedown",
		"arrowup": "up", "arrowdown": "down", "arrowleft": "left", "arrowright": "right",
		"control": "ctrl", "alt": "alt", "shift": "shift",
		"meta": "cmd", "command": "cmd", "super": "cmd",
		"capslock": "capslock", "dead": "",
	}
	if v, ok := m[strings.ToLower(strings.TrimSpace(key))]; ok {
		return v
	}
	k := strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(k, "f") {
		if n, err := strconv.Atoi(strings.TrimPrefix(k, "f")); err == nil && n >= 1 && n <= 24 {
			return k
		}
	}
	return ""
}

func mapBrowserCode(code string) string {
	m := map[string]string{
		"ControlLeft": "lctrl", "ControlRight": "rctrl",
		"AltLeft": "lalt", "AltRight": "ralt",
		"ShiftLeft": "lshift", "ShiftRight": "rshift",
		"Space": "space", "Backspace": "backspace", "Escape": "escape",
		"Tab": "tab", "Enter": "enter", "NumpadEnter": "enter",
		"Delete": "delete", "Insert": "insert",
		"Home": "home", "End": "end", "PageUp": "pageup", "PageDown": "pagedown",
		"ArrowUp": "up", "ArrowDown": "down", "ArrowLeft": "left", "ArrowRight": "right",
	}
	if v, ok := m[code]; ok {
		return v
	}
	if strings.HasPrefix(code, "Key") && len(code) == 4 {
		return strings.ToLower(code[3:])
	}
	if strings.HasPrefix(code, "Digit") && len(code) == 6 {
		return code[5:]
	}
	if strings.HasPrefix(code, "F") {
		if n, err := strconv.Atoi(strings.TrimPrefix(code, "F")); err == nil && n >= 1 && n <= 24 {
			return strings.ToLower(code)
		}
	}
	return ""
}

func handleInput(data []byte) {
	var ev InputEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "mousemove":
		if x, y, ok := absXY(ev.X, ev.Y); ok {
			robotgo.MoveMouse(x, y)
		}
	case "mousedown":
		if x, y, ok := absXY(ev.X, ev.Y); ok {
			robotgo.MoveMouse(x, y)
		}
		robotgo.MouseToggle("down", btnName(ev.Button))
	case "mouseup":
		if x, y, ok := absXY(ev.X, ev.Y); ok {
			robotgo.MoveMouse(x, y)
		}
		robotgo.MouseToggle("up", btnName(ev.Button))
	case "scroll":
		dir := "down"
		if ev.DeltaY < 0 {
			dir = "up"
		}
		robotgo.ScrollMouse(3, dir)
	case "keydown":
		if k, ok := mapKey(ev); ok {
			robotgo.KeyToggle(k, "down")
		}
	case "keyup":
		if k, ok := mapKey(ev); ok {
			robotgo.KeyToggle(k, "up")
		}
	}
}

// ── Signaling message ─────────────────────────────────────────────────────────

type sigMsg struct {
	Type      string `json:"type"`
	Payload   string `json:"payload,omitempty"`
	Role      string `json:"role,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// ── WebRTC peer connection ────────────────────────────────────────────────────

func newPC() (*webrtc.PeerConnection, error) {
	return webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	})
}

func run() error {
	conn, _, err := websocket.DefaultDialer.Dial(*serverURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	log.Println("[agent] connected to signaling server")

	if err := conn.WriteJSON(sigMsg{Type: "register", Role: "target", SessionID: *sessionID}); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	log.Printf("[agent] registered as target | session=%s | waiting for controller...", *sessionID)

	pc, err := newPC()
	if err != nil {
		return fmt.Errorf("peer connection: %w", err)
	}
	defer pc.Close()

	screenDC, err := pc.CreateDataChannel("screen", &webrtc.DataChannelInit{Ordered: boolPtr(false)})
	if err != nil {
		return fmt.Errorf("screen dc: %w", err)
	}

	inputDC, err := pc.CreateDataChannel("input", nil)
	if err != nil {
		return fmt.Errorf("input dc: %w", err)
	}

	inputDC.OnOpen(func() { log.Println("[agent] input channel open") })
	inputDC.OnMessage(func(m webrtc.DataChannelMessage) { handleInput(m.Data) })

	screenDC.OnOpen(func() {
		log.Printf("[agent] screen channel open — %d fps, quality %d", *captureFPS, *captureQuality)
		ticker := time.NewTicker(time.Second / time.Duration(*captureFPS))
		defer ticker.Stop()
		for range ticker.C {
			if screenDC.ReadyState() != webrtc.DataChannelStateOpen {
				return
			}
			frame, err := captureFrame()
			if err != nil || frame == nil {
				continue
			}
			screenDC.Send(frame) //nolint:errcheck
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		b, _ := json.Marshal(c.ToJSON())
		conn.WriteJSON(sigMsg{Type: "ice-candidate", Payload: string(b)}) //nolint:errcheck
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("[agent] WebRTC → %s", s)
	})

	for {
		var msg sigMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		log.Printf("[agent] ← %q", msg.Type)

		switch msg.Type {
		case "target-connected", "ready":
			offer, err := pc.CreateOffer(nil)
			if err != nil {
				log.Printf("[agent] CreateOffer: %v", err)
				continue
			}
			if err := pc.SetLocalDescription(offer); err != nil {
				log.Printf("[agent] SetLocalDescription: %v", err)
				continue
			}
			b, _ := json.Marshal(offer)
			conn.WriteJSON(sigMsg{Type: "offer", Payload: string(b)}) //nolint:errcheck
			log.Println("[agent] SDP offer sent")

		case "answer":
			var sdp webrtc.SessionDescription
			if err := json.Unmarshal([]byte(msg.Payload), &sdp); err != nil {
				continue
			}
			pc.SetRemoteDescription(sdp) //nolint:errcheck

		case "ice-candidate":
			var cand webrtc.ICECandidateInit
			if err := json.Unmarshal([]byte(msg.Payload), &cand); err != nil {
				continue
			}
			pc.AddICECandidate(cand) //nolint:errcheck

		case "controller-disconnected":
			log.Println("[agent] controller left")
		}
	}
}

func main() {
	flag.Parse()
	log.Printf("════════════════════════════════════════")
	log.Printf("  Target Agent | session=%s", *sessionID)
	log.Printf("  Server: %s", *serverURL)
	log.Printf("════════════════════════════════════════")
	for {
		if err := run(); err != nil {
			log.Printf("[agent] %v — retrying in %s", err, *retryDelay)
		}
		time.Sleep(*retryDelay)
	}
}

func boolPtr(b bool) *bool { return &b }
