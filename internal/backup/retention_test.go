package backup

import "testing"

func TestApplyRetentionEnvironment(t *testing.T) {
	t.Setenv(keepRecentEnvironment, "12")
	t.Setenv(keepDaysEnvironment, "14")
	configured, err := ApplyRetentionEnvironment(Config{KeepRecent: 48, KeepDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if configured.KeepRecent != 12 || configured.KeepDays != 14 {
		t.Fatalf("unexpected retention: %+v", configured)
	}
}

func TestApplyRetentionEnvironmentRejectsInvalidValues(t *testing.T) {
	t.Setenv(keepRecentEnvironment, "0")
	if _, err := ApplyRetentionEnvironment(Config{}); err == nil {
		t.Fatal("expected invalid recent retention to fail")
	}
	t.Setenv(keepRecentEnvironment, "")
	t.Setenv(keepDaysEnvironment, "not-a-number")
	if _, err := ApplyRetentionEnvironment(Config{}); err == nil {
		t.Fatal("expected invalid daily retention to fail")
	}
}
