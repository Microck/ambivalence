package protocol

import "testing"

func TestGenerateAndNormalizeCode(t *testing.T) {
	c, err := GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 6 {
		t.Fatalf("len %d", len(c))
	}
	got, err := NormalizeCode("  " + c[:3] + "-" + c[3:] + "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("got %s want %s", got, c)
	}
	if _, err := NormalizeCode("abc"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := NormalizeCode("OOOOOO"); err == nil {
		t.Fatal("O is not in alphabet")
	}
}
