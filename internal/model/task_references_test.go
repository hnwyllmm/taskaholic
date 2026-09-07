package model

import "testing"

func TestAntMulticaIssueURL(t *testing.T) {
	url, err := AntMulticaIssueURL("seekdb", "c1d82f86-9ec3-4c8a-a748-f086099b5215")
	if err != nil || url != "https://antmultica.alipay.com/seekdb/issues/c1d82f86-9ec3-4c8a-a748-f086099b5215" {
		t.Fatal(url, err)
	}
	for _, bad := range []string{"", "..", "../other", "bad?query", "bad#fragment", "a:b", "<script>"} {
		if _, err := AntMulticaIssueURL(bad, "one"); err == nil {
			t.Fatal("unsafe workspace", bad)
		}
		if _, err := AntMulticaIssueURL("seekdb", bad); err == nil {
			t.Fatal("unsafe issue", bad)
		}
	}
}
