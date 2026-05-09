package sample

import "testing"

func TestGreet(t *testing.T) {
	u := User{ID: 1, Name: "Felix", Email: "felix@example.com"}
	got := Greet(u)
	want := "Hello, Felix!"
	if got != want {
		t.Fatalf("Greet(%+v) = %q, want %q", u, got, want)
	}
}
