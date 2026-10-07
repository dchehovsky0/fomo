package proxy

import (
	"errors"
	"strings"
	"testing"
)

func TestParseHostPortUserPass(t *testing.T) {
	u, err := parse("10.0.0.1:8080:user:secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "socks5://user:secret@10.0.0.1:8080") {
		t.Fatalf("url = %s", u)
	}
	if _, err := parse("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRedact(t *testing.T) {
	got := Redact(errors.New(`Get "https://api10.axiom.trade/x": proxy http://user:secret@10.0.0.1:8080: refused`))
	if strings.Contains(got, "secret") || strings.Contains(got, "user:") {
		t.Fatalf("password leaked: %s", got)
	}
}
