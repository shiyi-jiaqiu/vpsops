package execd

import "testing"

func TestCappedBuffer(t *testing.T) {
	b := newCappedBuffer(5)
	n, err := b.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len("hello world") {
		t.Fatalf("expected full logical write count, got %d", n)
	}
	if b.String() != "hello" {
		t.Fatalf("unexpected content: %q", b.String())
	}
	if !b.Truncated() {
		t.Fatal("expected truncated flag")
	}
}
