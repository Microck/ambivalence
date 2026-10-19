package protocol

import (
	"encoding/json"
	"math"
	"testing"
)

func TestControlsValidation(t *testing.T) {
	n := func(v float64) *float64 { return &v }
	tests := []struct {
		name  string
		c     Controls
		valid bool
	}{
		{"infinity focus", Controls{FocusMode: "manual", FocusPosition: n(0)}, true},
		{"focus point", Controls{FocusMode: "auto", FocusPoint: &Point{X: 0, Y: 1}}, true},
		{"manual exposure", Controls{ExposureMode: "manual", ExposureTime: n(1.0 / 125), ISO: n(100)}, true},
		{"zero EV", Controls{ExposureMode: "auto", ExposureCompensation: n(0)}, true},
		{"auto reset", Controls{FocusMode: "auto", ExposureMode: "auto", WhiteBalanceMode: "auto"}, true},
		{"gains", Controls{WhiteBalanceMode: "manual", WhiteBalanceGains: &Gains{2, 1, 1.5}}, true},
		{"kelvin", Controls{WhiteBalanceMode: "manual", WhiteBalanceTemperature: n(5600)}, true},
		{"unknown mode", Controls{FocusMode: "banana"}, false},
		{"focus missing mode", Controls{FocusPosition: n(0)}, false},
		{"focus missing position", Controls{FocusMode: "manual"}, false},
		{"focus beyond lens", Controls{FocusMode: "manual", FocusPosition: n(1.1)}, false},
		{"manual focus point", Controls{FocusMode: "manual", FocusPosition: n(0.5), FocusPoint: &Point{0.5, 0.5}}, false},
		{"invalid point", Controls{FocusMode: "auto", FocusPoint: &Point{math.NaN(), 0.5}}, false},
		{"partial exposure", Controls{ExposureMode: "manual", ISO: n(100)}, false},
		{"auto shutter", Controls{ExposureMode: "auto", ExposureTime: n(0.01), ISO: n(100)}, false},
		{"negative shutter", Controls{ExposureMode: "manual", ExposureTime: n(-1), ISO: n(100)}, false},
		{"manual EV", Controls{ExposureMode: "manual", ExposureTime: n(0.01), ISO: n(100), ExposureCompensation: n(0)}, false},
		{"NaN", Controls{ExposureCompensation: n(math.NaN())}, false},
		{"Inf", Controls{ExposureCompensation: n(math.Inf(1))}, false},
		{"both WB inputs", Controls{WhiteBalanceMode: "manual", WhiteBalanceGains: &Gains{1, 1, 1}, WhiteBalanceTemperature: n(5000)}, false},
		{"missing WB input", Controls{WhiteBalanceMode: "manual"}, false},
		{"invalid gains", Controls{WhiteBalanceMode: "manual", WhiteBalanceGains: &Gains{0, 1, 1}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.c.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate()=%v, want valid=%v", err, tt.valid)
			}
		})
	}
}

func TestControlsWireZeroAndCapabilities(t *testing.T) {
	zero := 0.0
	b, err := Encode(Message{Type: TypeCommand, Controls: &Controls{FocusMode: "manual", FocusPosition: &zero, ExposureCompensation: &zero}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Decode(b)
	if err != nil || m.Controls.FocusPosition == nil || m.Controls.ExposureCompensation == nil {
		t.Fatalf("zero value lost: %s %v", b, err)
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if raw["controls"].(map[string]any)["focusPosition"] != float64(0) {
		t.Fatalf("bad controls: %s", b)
	}
	b, err = Encode(Message{Type: TypeCameras, Capabilities: map[string]any{"controlsVersion": 1}, Cameras: []Camera{{ID: "back/tele", Label: "Telephoto", Capabilities: map[string]any{"iso": map[string]any{"min": 100, "max": 1600}}}}})
	if err != nil {
		t.Fatal(err)
	}
	m, err = Decode(b)
	if err != nil || m.Capabilities["controlsVersion"] != float64(1) || m.Cameras[0].Label != "Telephoto" {
		t.Fatalf("metadata lost: %s %v", b, err)
	}
}
