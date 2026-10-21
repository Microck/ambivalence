package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"rsc.io/qr"

	"github.com/Microck/ambivalence/internal/audio"
	"github.com/Microck/ambivalence/internal/config"
	"github.com/Microck/ambivalence/internal/host"
	"github.com/Microck/ambivalence/internal/hub"
	"github.com/Microck/ambivalence/internal/mcp"
	"github.com/Microck/ambivalence/internal/peer"
	"github.com/Microck/ambivalence/internal/protocol"
)

const version = "0.3.0"

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	case "version", "--version":
		fmt.Println(version)
		return
	case "hub":
		err = cmdHub(args)
	case "mcp":
		err = cmdMCP(args)
	case "pair":
		err = cmdPair(args)
	case "capture":
		err = cmdCapture(args)
	case "listen":
		err = cmdListen(args)
	case "level":
		err = cmdLevel(args)
	case "status":
		err = cmdStatus(args)
	case "cameras":
		err = cmdCameras(args)
	case "torch":
		err = cmdTorch(args)
	case "switch":
		err = cmdSwitch(args)
	case "configure":
		err = cmdConfigure(args)
	case "zoom":
		err = cmdZoom(args)
	case "host":
		err = cmdHost(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ambivalence: give coding agents a phone camera and microphone

USAGE
  ambivalence <command> [flags]

COMMANDS
  hub          run the pairing hub + web host
  mcp          stdio MCP server (starts a hub unless --no-hub)
  pair CODE    connect the CLI as an agent
  capture      grab a JPEG from the paired host
  listen       record audio (a clip, or stream until Ctrl+C with --duration 0)
  level        measure microphone loudness in dBFS
  status       print pairing / camera state
  cameras      list cameras on the host
  torch on|off
  switch front|back|CAMERA_ID
  configure JSON  set manual controls (see docs/camera-controls.md)
  zoom FACTOR
  host fake    test-pattern camera host (no phone)
  version

The phone opens the hub URL (web host) or the native app, shows a 6-character
code, then the agent runs: ambivalence pair ABC123 && ambivalence capture

ENV
  AMBIVALENCE_HUB      ws://host:7842/v1
  AMBIVALENCE_LISTEN   0.0.0.0:7842
  AMBIVALENCE_PUBLIC_HUB   public wss URL advertised to the phone
`)
}

func cmdHub(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	listen := fs.String("listen", config.ListenAddr(""), "bind address")
	tunnel := fs.Bool("tunnel", false, "run cloudflared, then print the public URL and a QR code the phone can scan")
	public := fs.String("public", "", "public https URL of this hub (prints the phone QR code without --tunnel)")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h := hub.New(*listen)
	fmt.Fprintf(os.Stderr, "ambivalence hub listening on %s\n", *listen)
	switch {
	case *tunnel:
		go runTunnel(ctx, *listen)
	case *public != "":
		printPhoneLink(*public)
	default:
		fmt.Fprintf(os.Stderr, "phones on this Wi-Fi find this hub by themselves; for other networks rerun with --tunnel or --public https://…\n")
	}
	return h.Start(ctx)
}

// printPhoneLink shows what a phone needs to join: a QR code for the app deep link and the
// HTTPS page for the browser host. Scanning the QR with the system camera opens the app connected.
func printPhoneLink(httpsURL string) {
	httpsURL = strings.TrimRight(httpsURL, "/")
	wss := "wss://" + strings.TrimPrefix(httpsURL, "https://") + protocol.Path
	deep := "ambivalence://host?hub=" + url.QueryEscape(wss)
	fmt.Fprintf(os.Stderr, "\nscan with the phone camera to open the ambivalence app:\n\n%s\n", qrText(deep))
	fmt.Fprintf(os.Stderr, "no app? open %s in the phone browser\nhub for agents: %s\n\n", httpsURL, wss)
}

// qrText renders a QR code with half-block characters, two modules per text row,
// light on dark with a 2-module quiet zone so phone cameras pick it up from a terminal.
func qrText(s string) string {
	code, err := qr.Encode(s, qr.M)
	if err != nil {
		return s
	}
	n, q := code.Size, 2
	dark := func(x, y int) bool { return x >= 0 && y >= 0 && x < n && y < n && code.Black(x, y) }
	var b strings.Builder
	for y := -q; y < n+q; y += 2 {
		for x := -q; x < n+q; x++ {
			top, bot := !dark(x, y), !dark(x, y+1) // inverted: light modules print as blocks
			switch {
			case top && bot:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bot:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	listen := fs.String("listen", config.ListenAddr(""), "hub bind address")
	hubURL := fs.String("hub", "", "agent hub websocket URL")
	public := fs.String("public-hub", strings.TrimSpace(os.Getenv("AMBIVALENCE_PUBLIC_HUB")), "public wss URL for the phone")
	noHub := fs.Bool("no-hub", false, "do not start an in-process hub")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcp.Run(ctx, mcp.Options{
		Listen:    *listen,
		HubURL:    *hubURL,
		PublicHub: *public,
		StartHub:  !*noHub,
	})
}

func cmdPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	hubURL := fs.String("hub", config.HubURL(""), "hub websocket URL")
	timeout := fs.Duration("timeout", 2*time.Minute, "wait for the phone and your Confirm tap")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: ambivalence pair CODE   (only needed once; the phone stays linked)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	sess, err := peer.ConnectAgent(ctx, *hubURL, fs.Arg(0), showSAS)
	if err != nil {
		return err
	}
	defer sess.Close()
	fmt.Printf("linked via %s; from now on just run capture, no code needed\n", *hubURL)
	return nil
}

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	hubURL := fs.String("hub", config.HubURL(""), "hub websocket URL")
	code := fs.String("code", "", "pair code (only for the first pairing)")
	out := fs.String("o", "", "write JPEG to this path (default stdout if not a TTY, else captures/)")
	quality := fs.Int("quality", protocol.DefaultQuality, "JPEG quality")
	maxWidth := fs.Int("max-width", protocol.DefaultMaxWidth, "longest edge")
	_ = fs.Parse(args)
	sess, closer, err := agentSession(*hubURL, *code)
	if err != nil {
		return err
	}
	defer closer()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fr, err := sess.Capture(ctx, *quality, *maxWidth)
	if err != nil {
		return err
	}
	path := *out
	if path == "" {
		if fileInfo, _ := os.Stdout.Stat(); fileInfo != nil && fileInfo.Mode()&os.ModeCharDevice != 0 {
			_ = os.MkdirAll("captures", 0o755)
			path = filepath.Join("captures", time.Now().UTC().Format("20060102-150405")+".jpg")
		}
	}
	if path == "" || path == "-" {
		_, err = os.Stdout.Write(fr.Data)
		return err
	}
	if err := os.WriteFile(path, fr.Data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s %dx%d %d bytes\n", path, fr.Meta.Width, fr.Meta.Height, len(fr.Data))
	fmt.Println(path)
	return nil
}

func cmdListen(args []string) error {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	hubURL := fs.String("hub", config.HubURL(""), "hub websocket URL")
	code := fs.String("code", "", "pair code (only for the first pairing)")
	out := fs.String("o", "", "write WAV to this path (default stdout if not a TTY, else recordings/)")
	duration := fs.Duration("duration", 5*time.Second, "recording length; 0 streams until Ctrl+C")
	rate := fs.Int("rate", protocol.DefaultAudioSampleRate, "sample rate in Hz")
	_ = fs.Parse(args)
	if *duration < 0 {
		return fmt.Errorf("duration cannot be negative")
	}
	if *duration > 0 && *duration < 100*time.Millisecond {
		return fmt.Errorf("duration must be 0 or at least 100ms")
	}
	if *duration > time.Duration(protocol.MaxSessionMs)*time.Millisecond {
		return fmt.Errorf("duration %s exceeds the %s session limit; start another recording after it", *duration, time.Duration(protocol.MaxSessionMs)*time.Millisecond)
	}
	maxMs := int(*duration / time.Millisecond)
	sess, closer, err := agentSession(*hubURL, *code)
	if err != nil {
		return err
	}
	defer closer()

	// Short clips travel in one reply. Longer recordings (including --duration 0) pull chunks
	// so a phone can keep recording for hours without one giant message.
	if maxMs > 0 && maxMs <= protocol.MaxOneShotAudioMs {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(maxMs+15_000)*time.Millisecond)
		defer cancel()
		fr, err := sess.Listen(ctx, maxMs, *rate)
		if err != nil {
			return err
		}
		path := *out
		if path == "" {
			if info, _ := os.Stdout.Stat(); info != nil && info.Mode()&os.ModeCharDevice != 0 {
				_ = os.MkdirAll("recordings", 0o755)
				path = filepath.Join("recordings", time.Now().UTC().Format("20060102-150405")+".wav")
			}
		}
		if path == "" || path == "-" {
			_, err = os.Stdout.Write(fr.Data)
			return err
		}
		if err := os.WriteFile(path, fr.Data, 0o644); err != nil {
			return err
		}
		level := "rms=unknown peak=unknown"
		if fr.Meta.Levels != nil {
			level = fmt.Sprintf("rms=%.1fdB peak=%.1fdB", fr.Meta.Levels.RMSDB, fr.Meta.Levels.PeakDB)
		}
		fmt.Fprintf(os.Stderr, "wrote %s %.1fs %d Hz %s\n", path, float64(fr.Meta.DurationMs)/1000, fr.Meta.SampleRate, level)
		fmt.Println(path)
		return nil
	}

	return streamAudio(sess, *out, maxMs, *rate)
}

// audioSession is the subset of peer.Session that streamAudio drives; a fake implementation in
// tests can simulate Ctrl+C mid-chunk and mid-stream read errors.
type audioSession interface {
	AudioStart(ctx context.Context, maxDurationMs, sampleRate int) (protocol.Message, error)
	AudioRead(ctx context.Context, listenID string, chunkMs int) (peer.Frame, error)
	AudioStop(ctx context.Context, listenID string) (protocol.Message, error)
}

// streamAudio records a session, writing each chunk's PCM into one growing WAV file. With
// duration 0 it runs until Ctrl+C. The header is patched on exit when the destination can seek.
func streamAudio(sess audioSession, out string, maxMs, rate int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return streamAudioWithContext(ctx, sess, out, maxMs, rate, protocol.DefaultChunkMs)
}

func streamAudioWithContext(ctx context.Context, sess audioSession, out string, maxMs, rate, chunkMs int) error {
	started, err := sess.AudioStart(ctx, maxMs, rate)
	if err != nil {
		return err
	}
	stopSession := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = sess.AudioStop(sctx, started.ListenID)
	}
	path := out
	if path == "" {
		if info, _ := os.Stdout.Stat(); info != nil && info.Mode()&os.ModeCharDevice != 0 {
			_ = os.MkdirAll("recordings", 0o755)
			path = filepath.Join("recordings", time.Now().UTC().Format("20060102-150405")+".wav")
		}
	}
	var f *os.File
	var w io.Writer = os.Stdout
	var seeker io.WriteSeeker
	if path != "" && path != "-" {
		f, err = os.Create(path)
		if err != nil {
			stopSession()
			return err
		}
		defer f.Close()
		w, seeker = f, f
	} else if info, statErr := os.Stdout.Stat(); statErr == nil && info.Mode().IsRegular() {
		// A redirected stdout is a real file we can seek back into and patch. A pipe is not:
		// patching would fail after a successful recording, so leave its header at zero.
		seeker = os.Stdout
	}
	// One effective rate for the header and the summary: the host may correct or omit the rate,
	// and --rate 0 asks the host to choose.
	effRate := started.SampleRate
	if effRate == 0 {
		effRate = rate
	}
	if effRate == 0 {
		effRate = protocol.DefaultAudioSampleRate
	}
	sw := &wavStream{w: w, seeker: seeker, rate: effRate}
	if err := sw.start(); err != nil {
		stopSession()
		return err
	}
	// Always patch the WAV header on exit so any chunks already written remain a valid WAV file
	// even if a later network read fails.
	defer func() { _ = sw.close() }()

	// The phone stops recording at maxMs on its own; keep draining until it reports ended so a
	// tail still in flight is not lost to the local clock.
	deadline := audioDrainDeadline(maxMs, time.Now())
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		if ctx.Err() != nil {
			break
		}
		// Keep the in-flight RPC decoupled from ctx cancellation so Ctrl+C does not abandon the
		// phone's current audio_read chunk; instead, wait up to the chunk timeout to write the
		// current chunk before stopping.
		rctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		fr, err := sess.AudioRead(rctx, started.ListenID, chunkMs)
		cancel()
		if err != nil {
			stopSession()
			if ctx.Err() != nil {
				break
			}
			return err
		}
		pcm, err := audio.PCMFromWAV(fr.Data)
		if err != nil {
			stopSession()
			return err
		}
		if err := sw.write(pcm); err != nil {
			stopSession()
			return err
		}
		if fr.Meta.Ended || ctx.Err() != nil {
			break
		}
	}
	stopSession()
	if err := sw.close(); err != nil {
		return err
	}
	if path != "" && path != "-" {
		fmt.Fprintf(os.Stderr, "wrote %s %.1fs %d Hz\n", path, float64(sw.dataBytes)/(float64(effRate)*2), effRate)
		fmt.Println(path)
	}
	return nil
}

// audioDrainGrace is how long past the requested cap we keep waiting for the phone to report the
// session ended. The cap is enforced on the phone, so this only bounds a host that never ends.
const audioDrainGrace = 60 * time.Second

// audioDrainDeadline returns the local backstop for a streaming recording. It sits well past the
// phone's own cap so the buffered tail is drained, not truncated by the agent's clock. A zero cap
// (record until interrupted) has no backstop and returns the zero time.
func audioDrainDeadline(maxMs int, now time.Time) time.Time {
	if maxMs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(maxMs)*time.Millisecond + audioDrainGrace)
}

// wavStream writes a canonical header up front and patches its size fields when the output
// can seek. A pipe gets a zero-length header, which is all a streaming reader can use anyway.
type wavStream struct {
	w         io.Writer
	seeker    io.WriteSeeker
	rate      int
	dataBytes int
}

func (s *wavStream) start() error {
	_, err := s.w.Write(audio.WAVHeader(0, s.rate))
	return err
}

func (s *wavStream) write(pcm []byte) error {
	n, err := s.w.Write(pcm)
	s.dataBytes += n
	return err
}

func (s *wavStream) close() error {
	if s.seeker == nil {
		return nil
	}
	if _, err := s.seeker.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := s.seeker.Write(audio.WAVHeader(s.dataBytes, s.rate))
	return err
}

func cmdLevel(args []string) error {
	fs := flag.NewFlagSet("level", flag.ExitOnError)
	hubURL := fs.String("hub", config.HubURL(""), "hub websocket URL")
	code := fs.String("code", "", "pair code (only for the first pairing)")
	duration := fs.Duration("duration", time.Second, "measurement window")
	_ = fs.Parse(args)
	if *duration < 100*time.Millisecond || *duration > protocol.MaxChunkMs*time.Millisecond {
		return fmt.Errorf("duration must be between 100ms and %s", time.Duration(protocol.MaxChunkMs)*time.Millisecond)
	}
	sess, closer, err := agentSession(*hubURL, *code)
	if err != nil {
		return err
	}
	defer closer()
	ctx, cancel := context.WithTimeout(context.Background(), *duration+15*time.Second)
	defer cancel()
	fr, err := sess.Level(ctx, int(*duration/time.Millisecond))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(fr.Meta)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	hubURL := fs.String("hub", config.HubURL(""), "hub")
	_ = fs.Parse(args)
	cfg, _ := config.Load()
	out := map[string]any{"hub": *hubURL, "linked": cfg.Device != "", "config": mustConfigPath()}
	if cfg.Device != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sess, err := peer.ConnectAgent(ctx, *hubURL, "", nil)
		if err == nil {
			defer sess.Close()
			fr, err := sess.Cameras(ctx)
			if err == nil {
				out["online"] = true
				out["cameras"] = fr.Meta.Cameras
				out["facing"] = fr.Meta.Facing
				out["cameraId"] = fr.Meta.CameraID
				out["settings"] = fr.Meta.Settings
				out["capabilities"] = fr.Meta.Capabilities
				out["torch"] = fr.Meta.Torch
				out["host"] = fr.Meta.Host
			} else {
				out["error"] = err.Error()
			}
		} else {
			out["online"] = false
			out["error"] = err.Error()
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func cmdCameras(args []string) error {
	return withAgent(args, func(sess *peer.Session) error {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		fr, err := sess.Cameras(ctx)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(fr.Meta)
	})
}

func cmdTorch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ambivalence torch on|off")
	}
	on := args[0] == "on" || args[0] == "1" || args[0] == "true"
	return runCommand(args[1:], protocol.Message{TorchOn: &on}, func(st protocol.Message) {
		fmt.Printf("torch=%v\n", st.Torch)
	})
}

func cmdSwitch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ambivalence switch front|back|CAMERA_ID")
	}
	return runCommand(args[1:], protocol.Message{Camera: args[0]}, func(st protocol.Message) {
		fmt.Printf("camera=%s facing=%s\n", st.CameraID, st.Facing)
	})
}

func cmdZoom(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ambivalence zoom FACTOR")
	}
	var z float64
	if _, err := fmt.Sscanf(args[0], "%f", &z); err != nil {
		return err
	}
	return runCommand(args[1:], protocol.Message{ZoomLevel: &z}, func(st protocol.Message) {
		fmt.Printf("zoom=%.2f\n", st.Zoom)
	})
}

// runCommand sends one hardware command to the linked phone and prints its resulting status.
func runCommand(args []string, msg protocol.Message, report func(status protocol.Message)) error {
	return withAgent(args, func(sess *peer.Session) error {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		fr, err := sess.Command(ctx, msg)
		if err != nil {
			return err
		}
		report(fr.Meta)
		return nil
	})
}

func cmdHost(args []string) error {
	mode := "fake"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	hubURL := fs.String("hub", config.HubURL(""), "hub websocket URL")
	name := fs.String("name", "fake", "host name")
	state := fs.String("state", "", "file keeping the fake phone's device id and link key across runs (like app storage)")
	_ = fs.Parse(args)
	if mode != "fake" {
		return fmt.Errorf("unknown host mode %q (want fake)", mode)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	phone, err := host.LoadPhone(*state, *name)
	if err != nil {
		return err
	}
	return phone.Serve(ctx, *hubURL, func(code string) { fmt.Printf("code %s\n", code) })
}

func withAgent(args []string, fn func(*peer.Session) error) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hubURL := fs.String("hub", config.HubURL(""), "hub")
	code := fs.String("code", "", "code")
	_ = fs.Parse(args)
	sess, closer, err := agentSession(*hubURL, *code)
	if err != nil {
		return err
	}
	defer closer()
	return fn(sess)
}

// agentSession joins the linked phone (waiting up to 30s for it to come online), or pairs
// and links when a code is given.
func agentSession(hubURL, code string) (*peer.Session, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	sess, err := peer.ConnectAgent(ctx, hubURL, code, showSAS)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return sess, func() { cancel(); _ = sess.Close() }, nil
}

// showSAS prints the pairing confirmation code; the phone shows the same code with a Confirm button.
func showSAS(sas string) {
	fmt.Fprintf(os.Stderr, "check that the phone shows %s, then tap Confirm on the phone\n", sas)
}

func mustConfigPath() string {
	p, _ := config.Path()
	return p
}

func runTunnel(ctx context.Context, listen string) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		log.Printf("tunnel: %v", err)
		return
	}
	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		log.Printf("tunnel: cloudflared not on PATH")
		return
	}
	cmd := exec.CommandContext(ctx, bin, "tunnel", "--no-autoupdate", "--url", "http://127.0.0.1:"+port)
	out, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("tunnel: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("tunnel: %v", err)
		return
	}
	// cloudflared prints its banner to stderr; pick out the quick-tunnel URL once.
	re := regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
	sc := bufio.NewScanner(out)
	shown := false
	for sc.Scan() {
		if m := re.FindString(sc.Text()); m != "" && !shown {
			shown = true
			printPhoneLink(m)
		}
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		log.Printf("tunnel: %v", err)
	}
}

func cmdConfigure(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ambivalence configure '{\"focusMode\":\"auto\"}'")
	}
	var controls protocol.Controls
	dec := json.NewDecoder(strings.NewReader(args[0]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&controls); err != nil {
		return err
	}
	if err := controls.Validate(); err != nil {
		return err
	}
	return runCommand(args[1:], protocol.Message{Controls: &controls}, func(st protocol.Message) { b, _ := json.MarshalIndent(st, "", "  "); fmt.Println(string(b)) })
}
