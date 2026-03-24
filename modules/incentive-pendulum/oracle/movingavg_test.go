package oracle

import "testing"

func TestMovingAverageRing(t *testing.T) {
	m := NewMovingAverageRing(3)
	m.Push(1)
	m.Push(2)
	mu, ok := m.Mean()
	if !ok || mu != 1.5 {
		t.Fatal(mu, ok)
	}
	m.Push(3)
	mu, ok = m.Mean()
	if !ok || mu != 2 {
		t.Fatal(mu)
	}
	m.Push(10) // drops 1
	mu, ok = m.Mean()
	if !ok || mu != 5 {
		t.Fatalf("want 5 got %v", mu)
	}
	m.Reset()
	if _, ok := m.Mean(); ok {
		t.Fatal()
	}
}

func TestMovingAverageRingIgnoresBad(t *testing.T) {
	m := NewMovingAverageRing(2)
	m.Push(-1)
	m.Push(0)
	if _, ok := m.Mean(); ok {
		t.Fatal()
	}
	m.Push(4)
	mu, ok := m.Mean()
	if !ok || mu != 4 {
		t.Fatal()
	}
}
