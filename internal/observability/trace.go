package observability

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

func EnsureTraceparent(in string) (traceparent, traceID string) {
	parts := strings.Split(in, "-")
	if len(parts) == 4 && len(parts[1]) == 32 && len(parts[2]) == 16 {
		return in, parts[1]
	}
	trace := make([]byte, 16)
	span := make([]byte, 8)
	_, _ = rand.Read(trace)
	_, _ = rand.Read(span)
	tid := hex.EncodeToString(trace)
	return "00-" + tid + "-" + hex.EncodeToString(span) + "-01", tid
}
