package booking

import "crypto/rand"

// pnrAlphabet has 32 characters: A–Z and 2–9, MINUS the look-alikes I, O, 0
// and 1, so a PNR read over the phone ("is that O or zero?") is never ambiguous.
// 24 letters + 8 digits = 32.
const pnrAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewPNR returns a random 6-character booking reference such as "K7QX2M".
//
// HOW MANY ARE THERE? 32^6 = 1,073,741,824 (about a billion). By the BIRTHDAY
// PARADOX, collisions become likely around √(32^6) ≈ 33,000 bookings, which is
// why the database's PRIMARY KEY rejects duplicates and the service retries
// with a fresh code (see Service.Confirm). Real airlines also recycle PNRs
// after the flight.
//
// WHY crypto/rand AND NOT math/rand? PNRs are used to look up and cancel
// bookings. math/rand is predictable: someone could guess other people's PNRs.
// crypto/rand comes from the operating system's secure generator.
//
// WHY `b & 31` IS FAIR: a random byte is 0–255. 256 is an exact multiple of 32,
// so keeping the low 5 bits (b & 31 = b % 32) makes every character exactly
// equally likely. With a 36-character alphabet, b % 36 would favour the first
// 4 characters (256 = 7×36 + 4): "modulo bias".
func NewPNR() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	out := make([]byte, 6)
	for i, b := range buf {
		out[i] = pnrAlphabet[b&31]
	}
	return string(out), nil
}

// NewID returns a random 128-bit id as 32 hex characters, for hold ids.
// Like a UUID: unguessable, and the chance of a collision is negligible.
func NewID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 32)
	for i, b := range buf {
		out[2*i], out[2*i+1] = hex[b>>4], hex[b&15] // high 4 bits, low 4 bits
	}
	return string(out), nil
}
