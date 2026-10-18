package peer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Microck/ambivalence/internal/config"
	"github.com/Microck/ambivalence/internal/link"
	"github.com/Microck/ambivalence/internal/protocol"
)

// ConnectAgent is the one way the CLI and MCP reach a phone.
//
// With a code it pairs: show receives the confirmation code that the phone also displays, and
// the call returns once the user taps Confirm; the link (device id + key) is saved so every later
// call passes an empty code. Without a code it joins the linked phone, waits up to ctx's deadline
// for the phone to come online (the user may open the app after asking), and returns only after
// both sides have proven the link key.
func ConnectAgent(ctx context.Context, hubURL, code string, show func(sas string)) (*Session, error) {
	cfg, _ := config.Load()
	if code != "" {
		code, err := protocol.NormalizeCode(code)
		if err != nil {
			return nil, err
		}
		s, err := Dial(ctx, DialOptions{Role: protocol.RoleAgent, Code: code, HubURL: hubURL})
		if err != nil {
			return nil, err
		}
		device, key, err := s.Pair(ctx, show)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("pairing failed: %w", err)
		}
		cfg.Hub, cfg.Device, cfg.Key = hubURL, device, link.Hex(key)
		if err := config.Save(cfg); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
	if cfg.Device == "" || cfg.Key == "" {
		return nil, fmt.Errorf("no linked phone yet: open the ambivalence app and pair once with the code it shows")
	}
	key, err := link.Unhex(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("saved link key is corrupt; pair again: %w", err)
	}
	for {
		s, err := Dial(ctx, DialOptions{Role: protocol.RoleAgent, Device: cfg.Device, Key: key, HubURL: hubURL})
		if err == nil {
			if err = s.WaitAuthed(ctx); err == nil {
				return s, nil
			}
			s.Close()
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w (gave up waiting)", ErrPhoneOffline)
		}
		if !errors.Is(err, ErrPhoneOffline) && !errors.Is(err, ErrPhoneBusy) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (gave up waiting)", ErrPhoneOffline)
		case <-time.After(time.Second):
		}
	}
}
