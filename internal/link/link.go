// Package link is the end-to-end trust between a phone and its agent. Hubs only relay: they
// never see a secret, so a rogue or unknown hub cannot impersonate either side.
//
// Pairing (once, numeric comparison like Bluetooth):
//
//	agent -> phone  pair1 {commit = H("commit", agentPub, nA)}
//	phone -> agent  pair2 {pub = phonePub, nonce = nB}
//	agent -> phone  pair3 {pub = agentPub, nonce = nA}      phone checks the commit
//	both show SAS = 6 chars of H("sas", agentPub, phonePub, nA, nB); the user checks they match
//	phone -> agent  pair4 {mac = HMAC(key, "confirm")}       after the user taps Confirm
//
// key = H("key", ECDH(P-256), agentPub, phonePub). The commit stops a relay from picking its key
// after seeing the other side's nonce, so a man in the middle wins only by guessing the 30-bit SAS
// in one live attempt; there is nothing to brute-force offline.
//
// Every later session: phone -> agent auth {nonce = nP}; agent -> phone auth {nonce = nP,
// agentNonce = nA, mac = HMAC(key, "agent", device, nP, nA)}; phone -> agent authok {mac =
// HMAC(key, "phone", device, nP, nA)}. After pairing, nP = nB and the nA of pair3.
//
// The handshake alone would let a hub relay one genuine exchange and then inject requests or
// forge frames, so all camera traffic after it is sealed (see Channel): AES-256-GCM under
// per-direction keys from H("session", key, device, nP, nA), with a strictly increasing sequence
// number as nonce that must arrive exactly in order. That authenticates every request, reply and
// frame, binds each JPEG to its metadata, blocks replay, drop and splice (also across
// sessions, as both nonces are fresh), and keeps frames private from the hub. Only hub-originated
// status, ping and errors are unsealed; forging those can deny service, nothing more.
//
// Hashes are SHA-256 over length-prefixed fields; byte strings travel as lowercase hex.
// The same construction is implemented in the Android, iOS and web hosts; keep them in sync.
package link

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

const sasAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// H is SHA-256 over "ambivalence-v1/"+label followed by each field as a 4-byte big-endian length
// and the bytes. Length prefixes make the encoding unambiguous.
func H(label string, fields ...[]byte) []byte {
	h := sha256.New()
	h.Write([]byte("ambivalence-v1/" + label))
	for _, f := range fields {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f)))
		h.Write(n[:])
		h.Write(f)
	}
	return h.Sum(nil)
}

// MAC is HMAC-SHA256(key, H(label, fields...)).
func MAC(key []byte, label string, fields ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(H(label, fields...))
	return m.Sum(nil)
}

// SAS renders the first 30 bits of H("sas", ...) as 6 characters of the pair-code alphabet.
func SAS(agentPub, phonePub, nA, nB []byte) string {
	d := H("sas", agentPub, phonePub, nA, nB)
	v := uint32(d[0])<<24 | uint32(d[1])<<16 | uint32(d[2])<<8 | uint32(d[3])
	out := make([]byte, 6)
	for i := range out {
		out[i] = sasAlphabet[(v>>(27-5*uint(i)))&31]
	}
	return string(out)
}

// Key derives the long-term link key from the ECDH secret and both public keys.
func Key(shared, agentPub, phonePub []byte) []byte { return H("key", shared, agentPub, phonePub) }

func Nonce() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS RNG failing is unrecoverable
	}
	return b
}

// Party is one side's ephemeral P-256 key for a pairing.
type Party struct {
	priv *ecdh.PrivateKey
	Pub  []byte // uncompressed point, 65 bytes
}

func NewParty() (*Party, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Party{priv: priv, Pub: priv.PublicKey().Bytes()}, nil
}

// Shared runs ECDH with the peer's uncompressed public key.
func (p *Party) Shared(peerPub []byte) ([]byte, error) {
	pk, err := ecdh.P256().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("bad peer key: %w", err)
	}
	return p.priv.ECDH(pk)
}

// Equal compares MACs in constant time.
func Equal(a, b []byte) bool { return hmac.Equal(a, b) }

// Hex and Unhex are the wire encoding for byte fields.
func Hex(b []byte) string { return hex.EncodeToString(b) }

func Unhex(s string) ([]byte, error) { return hex.DecodeString(s) }

// Channels returns the agent->phone and phone->agent sealing keys for one session.
func Channels(key []byte, device string, nP, nA []byte) (toPhone, toAgent []byte) {
	sk := H("session", key, []byte(device), nP, nA)
	return H("seal-agent", sk), H("seal-phone", sk)
}

// Channel seals or opens one direction. A box is seq (8 bytes, big-endian) || AES-GCM ciphertext,
// with nonce 4 zero bytes || seq and the seq as additional data. Open accepts only seq = last+1.
// Seal and Open are not safe for concurrent use; the sender must keep seal order equal to wire order.
type Channel struct {
	aead cipher.AEAD
	seq  uint64 // sealer: last used; opener: last accepted
}

func NewChannel(key []byte) *Channel {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // keys come from H: always 32 bytes
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Channel{aead: aead}
}

func (c *Channel) Seal(plain []byte) []byte {
	c.seq++
	box := binary.BigEndian.AppendUint64(make([]byte, 0, 8+len(plain)+16), c.seq)
	return c.aead.Seal(box, nonce(c.seq), plain, box[:8])
}

func (c *Channel) Open(box []byte) ([]byte, error) {
	if len(box) < 8+16 {
		return nil, errors.New("sealed message too short")
	}
	// Exactly the next number: the relay path is ordered and never drops legitimately, so any gap
	// or repeat is a hub replaying, dropping or splicing (e.g. one reply's JPEG onto another's
	// metadata). This is what binds a frame's JPEG box to the metadata box just before it.
	seq := binary.BigEndian.Uint64(box)
	if seq != c.seq+1 {
		return nil, errors.New("sealed message out of order (replayed, dropped or spliced)")
	}
	plain, err := c.aead.Open(nil, nonce(seq), box[8:], box[:8])
	if err != nil {
		return nil, errors.New("sealed message failed authentication")
	}
	c.seq = seq
	return plain, nil
}

func nonce(seq uint64) []byte {
	return binary.BigEndian.AppendUint64(make([]byte, 4, 12), seq)
}
