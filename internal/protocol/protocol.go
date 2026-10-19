// Package protocol is the wire contract between hub, agent, and camera hosts.
//
// Transport: JSON text frames on a WebSocket at /v1. The hub is an untrusted relay: after
// hello it forwards messages between the host and its agent, handling only its own control
// messages (ping from an unpaired client, and reject, which drops the agent). Camera traffic
// travels as end-to-end sealed messages (TypeSealed); a sealed frame is followed by one binary
// message holding the sealed JPEG. See package link and docs/protocol.md.
package protocol

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

const (
	Version = 1
	Path    = "/v1"

	DefaultListen   = "0.0.0.0:7842"
	DefaultQuality  = 80
	DefaultMaxWidth = 1280
	MaxJPEGBytes    = 12 << 20

	// ServiceType is the mDNS/Bonjour service a hub advertises on the local network.
	ServiceType = "_ambivalence._tcp"
	// ErrOffline is the hub's reply when a linked agent's phone is not connected; agents retry on it.
	ErrOffline = "linked phone is offline: open the ambivalence app"
	// ErrBusy: another agent holds the phone's slot (or a dead one has not timed out yet); retry.
	ErrBusy = "phone is busy with another agent connection"
	// ErrUnauthenticated: the phone has not (re)verified this agent yet.
	ErrUnauthenticated = "agent not authenticated"
)

type Role string

const (
	RoleHost  Role = "host"
	RoleAgent Role = "agent"
)

type Type string

const (
	TypeHello   Type = "hello"
	TypeWelcome Type = "welcome"
	TypeError   Type = "error"
	TypePing    Type = "ping"
	TypePong    Type = "pong"
	TypeCapture Type = "capture"
	TypeFrame   Type = "frame"
	TypeStatus  Type = "status"
	TypeCommand Type = "command"
	TypeCameras Type = "cameras"
	// Pairing and session auth between phone and agent; see package link. Hubs relay these
	// like any other message and never hold a secret.
	TypePair1  Type = "pair1"
	TypePair2  Type = "pair2"
	TypePair3  Type = "pair3"
	TypePair4  Type = "pair4"
	TypeAuth   Type = "auth"
	TypeAuthOK Type = "authok"
	// TypeReject: phone tells the hub to drop an agent that failed auth, freeing the slot.
	TypeReject Type = "reject"
	// TypeSealed carries one end-to-end sealed message in Box (base64; see package link). A
	// sealed frame is followed by a binary message that is itself a sealed box of the JPEG.
	TypeSealed Type = "sealed"
)

type Message struct {
	V    int    `json:"v"`
	ID   string `json:"id,omitempty"`
	Type Type   `json:"type"`

	Role Role   `json:"role,omitempty"`
	Code string `json:"code,omitempty"`
	// HubID (welcome): random per hub process, so a phone can tell two URLs reach one hub.
	HubID string `json:"hubId,omitempty"`
	Err   string `json:"error,omitempty"`

	// Device is a phone's permanent public id; it locates the phone, it grants nothing.
	Device     string `json:"device,omitempty"`
	Pub        string `json:"pub,omitempty"`
	Nonce      string `json:"nonce,omitempty"`
	Commit     string `json:"commit,omitempty"`
	MAC        string `json:"mac,omitempty"`
	AgentNonce string `json:"agentNonce,omitempty"`
	Box        string `json:"box,omitempty"`

	Quality  int `json:"quality,omitempty"`
	MaxWidth int `json:"maxWidth,omitempty"`

	Mime     string  `json:"mime,omitempty"`
	Bytes    int     `json:"bytes,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Facing   string  `json:"facing,omitempty"`
	CameraID string  `json:"cameraId,omitempty"`
	Torch    bool    `json:"torch,omitempty"`
	Zoom     float64 `json:"zoom,omitempty"`

	Controls     *Controls      `json:"controls,omitempty"`
	Settings     map[string]any `json:"settings,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
	Camera       string         `json:"camera,omitempty"`
	TorchOn      *bool          `json:"torchOn,omitempty"`
	ZoomLevel    *float64       `json:"zoomLevel,omitempty"`

	Connected bool      `json:"connected,omitempty"`
	Cameras   []Camera  `json:"cameras,omitempty"`
	Host      *HostInfo `json:"host,omitempty"`
}

type Camera struct {
	ID           string         `json:"id"`
	Label        string         `json:"label,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
	Facing       string         `json:"facing"`
	Torch        bool           `json:"torch"`
	MinZoom      float64        `json:"minZoom"`
	MaxZoom      float64        `json:"maxZoom"`
}

type HostInfo struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

func Encode(m Message) ([]byte, error) {
	if m.V == 0 {
		m.V = Version
	}
	return json.Marshal(m)
}

func Decode(b []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return Message{}, err
	}
	if m.V != Version {
		return Message{}, fmt.Errorf("unsupported protocol version %d", m.V)
	}
	return m, nil
}

const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

var codeRe = regexp.MustCompile(`^[23456789ABCDEFGHJKLMNPQRSTUVWXYZ]{6}$`)

func GenerateCode() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 6)
	for i, b := range buf {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

func NormalizeCode(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	if !codeRe.MatchString(s) {
		return "", fmt.Errorf("pair code must be 6 characters from %s", codeAlphabet)
	}
	return s, nil
}

// Controls change independent groups; omitted groups retain their configuration.
// Manual exposure always specifies shutter time AND ISO. Zero remains a value.
type Controls struct {
	FocusMode               string   `json:"focusMode,omitempty" jsonschema:"auto or manual"`
	FocusPosition           *float64 `json:"focusPosition,omitempty" jsonschema:"normalized lens position: 0 infinity, 1 nearest; requires manual"`
	FocusPoint              *Point   `json:"focusPoint,omitempty" jsonschema:"normalized sensor coordinates; requires auto focus"`
	ExposureMode            string   `json:"exposureMode,omitempty" jsonschema:"auto or manual"`
	ExposureTime            *float64 `json:"exposureTime,omitempty" jsonschema:"shutter duration in seconds; requires manual and iso"`
	ISO                     *float64 `json:"iso,omitempty" jsonschema:"sensor ISO; requires manual and exposureTime"`
	ExposureCompensation    *float64 `json:"exposureCompensation,omitempty" jsonschema:"EV bias; use auto exposure"`
	WhiteBalanceMode        string   `json:"whiteBalanceMode,omitempty" jsonschema:"auto or manual"`
	WhiteBalanceGains       *Gains   `json:"whiteBalanceGains,omitempty" jsonschema:"RGB gains; requires manual; native apps only"`
	WhiteBalanceTemperature *float64 `json:"whiteBalanceTemperature,omitempty" jsonschema:"kelvin; requires manual; iOS and supported browsers"`
}
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}
type Gains struct {
	Red   float64 `json:"red"`
	Green float64 `json:"green"`
	Blue  float64 `json:"blue"`
}

func (c Controls) Validate() error {
	for _, mode := range []string{c.FocusMode, c.ExposureMode, c.WhiteBalanceMode} {
		if mode != "" && mode != "auto" && mode != "manual" {
			return fmt.Errorf("mode must be auto or manual")
		}
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	for _, v := range []*float64{c.FocusPosition, c.ExposureTime, c.ISO, c.ExposureCompensation, c.WhiteBalanceTemperature} {
		if v != nil && !finite(*v) {
			return fmt.Errorf("controls must be finite")
		}
	}
	if c.FocusPosition != nil && (c.FocusMode != "manual" || *c.FocusPosition < 0 || *c.FocusPosition > 1) {
		return fmt.Errorf("focusPosition requires manual focus and a value in [0,1]")
	}
	if c.FocusMode == "manual" && c.FocusPosition == nil {
		return fmt.Errorf("manual focus requires focusPosition")
	}
	if p := c.FocusPoint; p != nil && (c.FocusMode != "auto" || !finite(p.X) || !finite(p.Y) || p.X < 0 || p.X > 1 || p.Y < 0 || p.Y > 1) {
		return fmt.Errorf("focusPoint requires auto focus and coordinates in [0,1]")
	}
	if c.ExposureMode == "manual" {
		if c.ExposureTime == nil || c.ISO == nil || *c.ExposureTime <= 0 || *c.ISO <= 0 || c.ExposureCompensation != nil {
			return fmt.Errorf("manual exposure requires positive exposureTime and iso, without EV compensation")
		}
	} else if c.ExposureTime != nil || c.ISO != nil {
		return fmt.Errorf("exposureTime and iso require manual exposure")
	}
	if c.WhiteBalanceMode == "manual" {
		if (c.WhiteBalanceGains == nil) == (c.WhiteBalanceTemperature == nil) {
			return fmt.Errorf("manual white balance requires exactly one of gains or temperature")
		}
	} else if c.WhiteBalanceGains != nil || c.WhiteBalanceTemperature != nil {
		return fmt.Errorf("white balance values require manual mode")
	}
	if g := c.WhiteBalanceGains; g != nil && (!finite(g.Red) || !finite(g.Green) || !finite(g.Blue) || g.Red < 1 || g.Green < 1 || g.Blue < 1) {
		return fmt.Errorf("RGB gains must be finite and >= 1")
	}
	if c.WhiteBalanceTemperature != nil && *c.WhiteBalanceTemperature <= 0 {
		return fmt.Errorf("temperature must be positive")
	}
	return nil
}
