package oracle

import "testing"

func TestMovingAverageRing(t *testing.T) {
	m := NewMovingAverageRing(3)
	m.Push(10)
	m.Push(20)
	mu, ok := m.Mean()
	if !ok || mu != 15 {
		t.Fatal(mu, ok)
	}
	m.Push(30)
	mu, ok = m.Mean()
	if !ok || mu != 20 {
		t.Fatal(mu)
	}
	m.Push(100) // drops 10
	mu, ok = m.Mean()
	if !ok || mu != 50 {
		t.Fatalf("want 50 got %d", mu)
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
	m.Push(40)
	mu, ok := m.Mean()
	if !ok || mu != 40 {
		t.Fatal()
	}
}
