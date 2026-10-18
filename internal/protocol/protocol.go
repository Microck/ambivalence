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

	Camera    string   `json:"camera,omitempty"`
	TorchOn   *bool    `json:"torchOn,omitempty"`
	ZoomLevel *float64 `json:"zoomLevel,omitempty"`

	Connected bool      `json:"connected,omitempty"`
	Cameras   []Camera  `json:"cameras,omitempty"`
	Host      *HostInfo `json:"host,omitempty"`
}

type Camera struct {
	ID      string  `json:"id"`
	Facing  string  `json:"facing"`
	Torch   bool    `json:"torch"`
	MinZoom float64 `json:"minZoom"`
	MaxZoom float64 `json:"maxZoom"`
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
