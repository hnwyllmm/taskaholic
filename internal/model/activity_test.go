package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestObservationMaskingAndBounds(t *testing.T) {
	input := `Authorization: Bearer bearer-example-value
{"api_key":"key-example-value","password":"password-example-value"}
curl --token cli-example-value
sk-1234567890abcdefghijklmnop`
	a := CleanAction(Action{ID: "safe", State: "RUNNING", Command: input, Output: strings.Repeat("中文输出", 5000)})
	for _, secret := range []string{"bearer-example-value", "key-example-value", "password-example-value", "cli-example-value", "sk-1234567890abcdefghijklmnop"} {
		if strings.Contains(a.Command, secret) {
			t.Fatalf("unmasked common secret %s", secret)
		}
	}
	if !a.Truncated || !a.Redacted || !utf8.ValidString(a.Output) || len(a.Output) > 16100 {
		t.Fatal("bad display bounds", len(a.Output), a.Truncated, a.Redacted)
	}
	if second := CleanAction(a); second.Command != a.Command || second.Output != a.Output {
		t.Fatal("cleaning is not idempotent")
	}
	negative := int64(-1)
	a = CleanAction(Action{State: "provider-private-state", DurationMS: &negative})
	if a.State != "UNKNOWN" || a.DurationMS != nil || a.Title == "" {
		t.Fatal(a)
	}
}
