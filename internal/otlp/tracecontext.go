package otlp

import (
	"encoding/hex"
	"math/rand/v2"
)

// TraceID and SpanID are W3C trace-context identifiers.
type (
	TraceID [16]byte
	SpanID  [8]byte
)

func (t TraceID) IsValid() bool  { return t != TraceID{} }
func (s SpanID) IsValid() bool   { return s != SpanID{} }
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }
func (s SpanID) String() string  { return hex.EncodeToString(s[:]) }

// FlagSampled is the trace-flags bit that says the caller records this trace.
const FlagSampled byte = 0x01

// SpanContext is the part of a span that crosses process boundaries.
type SpanContext struct {
	TraceID    TraceID
	SpanID     SpanID
	Flags      byte
	TraceState string // opaque, passed along unchanged
}

// IsValid reports whether both ids are set.
func (sc SpanContext) IsValid() bool { return sc.TraceID.IsValid() && sc.SpanID.IsValid() }

// Sampled reports whether the sampled flag is set.
func (sc SpanContext) Sampled() bool { return sc.Flags&FlagSampled != 0 }

// Traceparent formats sc as a version-00 W3C traceparent header value.
func (sc SpanContext) Traceparent() string {
	b := make([]byte, 0, 55)
	b = append(b, "00-"...)
	b = hex.AppendEncode(b, sc.TraceID[:])
	b = append(b, '-')
	b = hex.AppendEncode(b, sc.SpanID[:])
	b = append(b, '-')
	b = hex.AppendEncode(b, []byte{sc.Flags})
	return string(b)
}

// ParseTraceparent parses a W3C traceparent header. ok is false for anything
// malformed, all-zero ids, or the forbidden version ff; per the spec, the
// caller then starts a new trace.
func ParseTraceparent(s string) (sc SpanContext, ok bool) {
	if len(s) < 55 || s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return sc, false
	}
	ver, ok1 := lowerHex(s[0:2])
	if !ok1 || ver[0] == 0xff {
		return sc, false
	}
	// Version 00 is exactly 55 characters; later versions may append fields
	// after another dash, which this parser ignores.
	if (ver[0] == 0 && len(s) != 55) || (len(s) > 55 && s[55] != '-') {
		return sc, false
	}
	tid, ok1 := lowerHex(s[3:35])
	sid, ok2 := lowerHex(s[36:52])
	fl, ok3 := lowerHex(s[53:55])
	if !ok1 || !ok2 || !ok3 {
		return sc, false
	}
	copy(sc.TraceID[:], tid)
	copy(sc.SpanID[:], sid)
	sc.Flags = fl[0]
	return sc, sc.IsValid()
}

func lowerHex(s string) ([]byte, bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, false
		}
	}
	b, err := hex.DecodeString(s)
	return b, err == nil
}

// NewTraceID returns a random, non-zero trace id.
func NewTraceID() TraceID {
	var t TraceID
	for !t.IsValid() {
		putUint64(t[:8], rand.Uint64())
		putUint64(t[8:], rand.Uint64())
	}
	return t
}

// NewSpanID returns a random, non-zero span id.
func NewSpanID() SpanID {
	var s SpanID
	for !s.IsValid() {
		putUint64(s[:], rand.Uint64())
	}
	return s
}

func putUint64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (56 - 8*i))
	}
}
