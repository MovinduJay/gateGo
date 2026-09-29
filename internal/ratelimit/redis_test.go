package ratelimit

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadRESPArray(t *testing.T) {
	value, err := readRESP(bufio.NewReader(strings.NewReader("*3\r\n:1\r\n:7\r\n:250\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	values, ok := value.([]any)
	if !ok || len(values) != 3 || values[0] != int64(1) || values[1] != int64(7) || values[2] != int64(250) {
		t.Fatalf("unexpected RESP array: %#v", value)
	}
}
