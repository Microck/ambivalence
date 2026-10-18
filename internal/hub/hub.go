package hub

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grandcat/zeroconf"

	"github.com/Microck/ambivalence/internal/protocol"
)

//go:embed web
var webFS embed.FS

type Hub struct {
	Addr string
	// id is random per process. Phones use it to spot two URLs (e.g. tunnel and LAN) that reach
	// the same hub, which would otherwise evict each other's socket forever. Not a trust signal.
	id string

	mu       sync.Mutex
	rooms    map[string]*room
	byDevice map[string]*room // linked hosts, so an agent can join without a code
	ln       net.Listener
	srv      *http.Server
}

type room struct {
	code   string
	device string
	host   *client
	agent  *client
}

type client struct {
	role protocol.Role
	ws   *websocket.Conn
	mu   sync.Mutex // serialises writes to ws
	room *room      // current room; guarded by Hub.mu, moves when a phone reconnects
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func New(addr string) *Hub {
	if addr == "" {
		addr = protocol.DefaultListen
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &Hub{
		Addr:     addr,
		id:       hex.EncodeToString(id),
		rooms:    make(map[string]*room),
		byDevice: make(map[string]*room),
	}
}

func (h *Hub) ListenAddr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln != nil {
		return h.ln.Addr().String()
	}
	return h.Addr
}

func (h *Hub) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"name":    "ambivalence",
			"path":    protocol.Path,
			"version": protocol.Version,
		})
	})
	mux.HandleFunc(protocol.Path, h.handleWS)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	h.srv = &http.Server{
		Addr:              h.Addr,
		Handler:           withLog(mux),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	ln, err := net.Listen("tcp", h.Addr)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.ln = ln
	h.mu.Unlock()
	go h.advertise(ctx, ln.Addr())
	go func() {
		<-ctx.Done()
		shctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(shctx)
	}()
	err = h.srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	defer ws.Close()
	ws.SetReadLimit(protocol.MaxJPEGBytes + 64<<10)
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		return
	}
	if mt != websocket.TextMessage {
		writeErr(ws, "", "first message must be hello")
		return
	}
	msg, err := protocol.Decode(data)
	if err != nil {
		writeErr(ws, "", err.Error())
		return
	}
	if msg.Type != protocol.TypeHello {
		writeErr(ws, msg.ID, "first message must be hello")
		return
	}
	if msg.Role != protocol.RoleHost && msg.Role != protocol.RoleAgent {
		writeErr(ws, msg.ID, "role must be host or agent")
		return
	}
	c := &client{role: msg.Role, ws: ws}
	var rm *room
	var code string
	switch {
	case msg.Role == protocol.RoleHost:
		if len(msg.Device) < 16 {
			writeErr(ws, msg.ID, "host hello needs a device id")
			return
		}
		if code, err = protocol.GenerateCode(); err != nil {
			writeErr(ws, msg.ID, "could not allocate code")
			return
		}
		rm = h.joinHost(code, msg.Device, c)
	case msg.Device != "":
		// Linked agent: the device id only locates the phone. The phone then challenges the
		// agent end to end (package link); the hub authorises nothing.
		if rm, err = h.joinLinked(msg.Device, c); err != nil {
			writeErr(ws, msg.ID, err.Error())
			return
		}
		code = rm.code
	default:
		if code, err = protocol.NormalizeCode(msg.Code); err != nil {
			writeErr(ws, msg.ID, err.Error())
			return
		}
		if rm, err = h.joinCode(code, c); err != nil {
			writeErr(ws, msg.ID, err.Error())
			return
		}
	}
	defer h.leave(c)

	welcome := protocol.Message{
		V:         protocol.Version,
		ID:        msg.ID,
		Type:      protocol.TypeWelcome,
		Role:      msg.Role,
		Code:      code,
		Connected: rm.paired(),
		Host:      msg.Host,
		HubID:     h.id,
	}
	if err := c.writeJSON(welcome); err != nil {
		return
	}
	h.broadcastStatus(rm)

	// Dead peers (killed app, lost Wi-Fi) drop within a minute instead of holding the slot.
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				_ = ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		if c.role == protocol.RoleHost && mt == websocket.TextMessage {
			if m, err := protocol.Decode(data); err == nil && m.Type == protocol.TypeReject {
				h.dropAgent(c)
				continue
			}
		}
		// Looked up per message: a phone reconnect moves a linked agent into the new room.
		peer := h.peerOf(c)
		if peer == nil {
			if mt == websocket.TextMessage {
				m, decErr := protocol.Decode(data)
				if decErr == nil && m.Type == protocol.TypePing {
					_ = c.writeJSON(protocol.Message{V: protocol.Version, ID: m.ID, Type: protocol.TypePong})
					continue
				}
				if decErr == nil && m.ID != "" {
					reason := "peer not connected"
					if c.role == protocol.RoleAgent {
						reason = protocol.ErrOffline
					}
					_ = c.writeJSON(protocol.Message{V: protocol.Version, ID: m.ID, Type: protocol.TypeError, Err: reason})
				}
			}
			continue
		}
		if err := peer.writeRaw(mt, data); err != nil {
			return
		}
	}
}

// joinHost opens a fresh room for a phone. If the same device id was already present (app
// restart, network change) its old socket is closed and a waiting agent moves to the new room.
// The agent re-authenticates the new phone connection, so a host faking the id gets nothing.
func (h *Hub) joinHost(code, device string, c *client) *room {
	h.mu.Lock()
	defer h.mu.Unlock()
	rm := &room{code: code, device: device, host: c}
	if old := h.byDevice[device]; old != nil {
		if old.host != nil {
			old.host.room = nil
			old.host.ws.Close()
		}
		if old.agent != nil {
			rm.agent, old.agent.room = old.agent, rm
		}
		delete(h.rooms, old.code)
	}
	c.room = rm
	h.rooms[code] = rm
	h.byDevice[device] = rm
	return rm
}

func (h *Hub) joinCode(code string, c *client) (*room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rm := h.rooms[code]
	if rm == nil || rm.host == nil {
		return nil, fmt.Errorf("no phone is showing code %s", code)
	}
	if rm.agent != nil {
		return nil, fmt.Errorf("another agent is already using code %s", code)
	}
	rm.agent, c.room = c, rm
	return rm, nil
}

// joinLinked never evicts a connected agent: anyone can know a device id, so "newest wins"
// would let a stranger kick the real agent. A stale agent frees the slot when its socket times
// out, and the phone frees it at once with reject when an agent fails auth.
func (h *Hub) joinLinked(device string, c *client) (*room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rm := h.byDevice[device]
	if rm == nil || rm.host == nil {
		return nil, fmt.Errorf("%s", protocol.ErrOffline)
	}
	if rm.agent != nil {
		return nil, fmt.Errorf("%s", protocol.ErrBusy)
	}
	rm.agent, c.room = c, rm
	return rm, nil
}

// dropAgent disconnects the agent in host's room (the phone rejected it).
func (h *Hub) dropAgent(host *client) {
	h.mu.Lock()
	var a *client
	if rm := host.room; rm != nil && rm.host == host && rm.agent != nil {
		a = rm.agent
		rm.agent, a.room = nil, nil // free the slot now, not when the socket finishes closing
	}
	h.mu.Unlock()
	if a != nil {
		a.ws.Close()
	}
}

// leave detaches c. A room whose phone left but whose linked agent is still connected stays
// findable by device, so the phone can come back and pick the agent up.
func (h *Hub) leave(c *client) {
	h.mu.Lock()
	rm := c.room
	if rm == nil {
		h.mu.Unlock()
		return
	}
	c.room = nil
	if rm.host == c {
		rm.host = nil
		delete(h.rooms, rm.code) // the code dies with the phone connection
	}
	if rm.agent == c {
		rm.agent = nil
	}
	if rm.host == nil && rm.agent == nil && h.byDevice[rm.device] == rm {
		delete(h.byDevice, rm.device)
	}
	h.mu.Unlock()
	h.broadcastStatus(rm)
}

func (h *Hub) peerOf(c *client) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.room == nil {
		return nil
	}
	return c.room.peer(c)
}

func (h *Hub) broadcastStatus(rm *room) {
	h.mu.Lock()
	host, agent, paired := rm.host, rm.agent, rm.paired()
	h.mu.Unlock()
	msg := protocol.Message{V: protocol.Version, Type: protocol.TypeStatus, Code: rm.code, Connected: paired}
	if host != nil {
		_ = host.writeJSON(msg)
	}
	if agent != nil {
		_ = agent.writeJSON(msg)
	}
}

func (rm *room) paired() bool {
	return rm != nil && rm.host != nil && rm.agent != nil
}

func (rm *room) peer(c *client) *client {
	if rm.host == c {
		return rm.agent
	}
	return rm.host
}

func (c *client) writeJSON(msg protocol.Message) error {
	b, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	return c.writeRaw(websocket.TextMessage, b)
}

func (c *client) writeRaw(mt int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(20 * time.Second))
	return c.ws.WriteMessage(mt, data)
}

func writeErr(ws *websocket.Conn, id, err string) {
	b, _ := protocol.Encode(protocol.Message{V: protocol.Version, ID: id, Type: protocol.TypeError, Err: err})
	_ = ws.WriteMessage(websocket.TextMessage, b)
}

// advertise announces the hub on the local network as _ambivalence._tcp so phones on the
// same Wi-Fi find it without a URL. Skipped for loopback-only listeners, which no phone can reach.
//
// The TXT record carries addrs=<ipv4,...>. Phones connect to those directly instead of resolving
// the SRV hostname: that name is published by this library, the OS responder may own a different
// one, and in testing the hostname lookup never returned an address on macOS.
func (h *Hub) advertise(ctx context.Context, addr net.Addr) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP.IsLoopback() {
		return
	}
	ips := lanIPv4s()
	if len(ips) == 0 {
		return
	}
	name, _ := os.Hostname()
	txt := []string{"path=" + protocol.Path, fmt.Sprintf("v=%d", protocol.Version), "addrs=" + strings.Join(ips, ",")}
	srv, err := zeroconf.Register("ambivalence on "+name, protocol.ServiceType, "local.", tcp.Port, txt, nil)
	if err != nil {
		log.Printf("mdns: %v (phones on this Wi-Fi will not find the hub automatically)", err)
		return
	}
	<-ctx.Done()
	srv.Shutdown()
}

// lanIPv4s lists private IPv4 addresses of up, multicast-capable interfaces, skipping
// container and VPN bridges a phone on the Wi-Fi cannot reach.
func lanIPv4s() []string {
	ifaces, _ := net.Interfaces()
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if n := ifc.Name; strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "veth") ||
			strings.HasPrefix(n, "tailscale") || strings.HasPrefix(n, "utun") || strings.HasPrefix(n, "bridge") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil && ip4.IsPrivate() {
					out = append(out, ip4.String())
				}
			}
		}
	}
	return out
}
