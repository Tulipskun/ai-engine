package sdk

import "testing"

func TestKeyPoolRotates(t *testing.T) {
	p := NewKeyPool("a", "b", "c")
	got, _ := p.Current()
	if got != "a" {
		t.Fatal(got)
	}
	got, _ = p.Rotate()
	if got != "b" {
		t.Fatal(got)
	}
	got, _ = p.Rotate()
	if got != "c" {
		t.Fatal(got)
	}
	got, _ = p.Rotate()
	if got != "a" {
		t.Fatal(got)
	}
}

func TestKeyPoolIndexedAccessDoesNotMoveCursor(t *testing.T) {
	p := NewKeyPool("a", "b", "c")
	got, err := p.At(2)
	if err != nil || got != "c" {
		t.Fatalf("At(2) = %q, err=%v", got, err)
	}
	got, err = p.Current()
	if err != nil || got != "a" {
		t.Fatalf("Current() = %q, err=%v", got, err)
	}
}
