package crypto

import (
	"bytes"
	"testing"
)

func TestSealOpen(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	s, _ := New(key)
	blob, err := s.Seal([]byte("secret"))
	if err != nil || bytes.Contains(blob, []byte("secret")) {
		t.Fatalf("seal: %v", err)
	}
	out, err := s.Open(blob)
	if err != nil || string(out) != "secret" {
		t.Fatalf("open: %q %v", out, err)
	}
	plain, _ := New(nil)
	pb, _ := plain.Seal([]byte("x"))
	if out, _ := s.Open(pb); string(out) != "x" { // plain blobs stay readable after enabling a key
		t.Error("plain blob unreadable")
	}
	if _, err := plain.Open(blob); err == nil {
		t.Error("sealed blob opened without key")
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open(blob); err == nil {
		t.Error("opened with wrong key")
	}
}
