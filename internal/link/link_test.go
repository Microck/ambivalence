package link

import (
	"bytes"
	"testing"
)

// Fixed vectors pin the hash/MAC/SAS encoding. The Android, iOS and web hosts are checked
// against the same values (the same inputs are hard-coded in each host's self-test), so a change here breaks pairing.
func TestVectors(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 65)
	p := bytes.Repeat([]byte{2}, 65)
	nA := bytes.Repeat([]byte{3}, 16)
	nB := bytes.Repeat([]byte{4}, 16)
	if got := Hex(H("commit", a, nA)); got != wantCommit {
		t.Errorf("commit %s", got)
	}
	if got := SAS(a, p, nA, nB); got != wantSAS {
		t.Errorf("sas %s", got)
	}
	key := Key(bytes.Repeat([]byte{5}, 32), a, p)
	if got := Hex(key); got != wantKey {
		t.Errorf("key %s", got)
	}
	if got := Hex(MAC(key, "agent", []byte("dev"), nA)); got != wantAgentMAC {
		t.Errorf("agent mac %s", got)
	}
}

func TestPairingAgreesAndMITMChangesSAS(t *testing.T) {
	agent, _ := NewParty()
	phone, _ := NewParty()
	nA, nB := Nonce(), Nonce()
	s1, _ := agent.Shared(phone.Pub)
	s2, _ := phone.Shared(agent.Pub)
	if !bytes.Equal(Key(s1, agent.Pub, phone.Pub), Key(s2, agent.Pub, phone.Pub)) {
		t.Fatal("keys differ")
	}
	// A relay substituting its own key toward the phone produces a different SAS on each side.
	mitm, _ := NewParty()
	if SAS(agent.Pub, mitm.Pub, nA, nB) == SAS(mitm.Pub, phone.Pub, nA, nB) {
		t.Fatal("MITM SAS collided")
	}
}

const (
	wantCommit   = "c8f3be02f0acacde00678c4c449775ec01b7d712490e4c17e5c6c9b504e2544d"
	wantSAS      = "V7US6R"
	wantKey      = "cf76a39ef4c45ad928dbc050ebefe7ddd0b3b6f07c73d7b62de1676b7abf9b30"
	wantAgentMAC = "8851d815f3c73fb55dd47226a9b3d95bae443a00f0c872cd3bb0832044e954a9"
)

func TestChannel(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	toPhone, toAgent := Channels(key, "dev", bytes.Repeat([]byte{3}, 16), bytes.Repeat([]byte{4}, 16))
	if bytes.Equal(toPhone, toAgent) {
		t.Fatal("directions share a key")
	}
	tx, rx := NewChannel(toPhone), NewChannel(toPhone)
	b1, b2 := tx.Seal([]byte("hi")), tx.Seal([]byte("again"))
	// Pinned so the app implementations can check their channel against Go.
	if got := Hex(b1); got != "0000000000000001ee841b7f6af6a77bafbac9ed833fcb93b2ba" {
		t.Fatalf("box1 %s", got)
	}
	if got, err := rx.Open(b1); err != nil || string(got) != "hi" {
		t.Fatalf("open: %v", err)
	}
	if _, err := rx.Open(b1); err == nil {
		t.Fatal("replay accepted")
	}
	bad := append([]byte(nil), b2...)
	bad[len(bad)-1] ^= 1
	if _, err := rx.Open(bad); err == nil {
		t.Fatal("tampered box accepted")
	}
	if _, err := NewChannel(toAgent).Open(b2); err == nil {
		t.Fatal("box opened under the other direction's key")
	}
	if got, err := rx.Open(b2); err != nil || string(got) != "again" {
		t.Fatalf("open after rejects: %v", err)
	}
	// Splice: a hub drops box 3 and delivers box 4 in its place.
	tx.Seal([]byte("dropped"))
	if _, err := rx.Open(tx.Seal([]byte("spliced"))); err == nil {
		t.Fatal("gap accepted")
	}
}
