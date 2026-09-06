package localconfig

import (
	"strings"
	"testing"
)

func TestSupervisorAndLocalShareExposurePolicy(t *testing.T) {
	for listen, want := range map[string]string{"0.0.0.0:17343": "http://127.0.0.1:17343", "[::]:17343": "http://[::1]:17343", "127.0.0.1:17343": "http://127.0.0.1:17343"} {
		got, err := ControlURL(listen, true, true, "", strings.Repeat("r", 32))
		if err != nil || got != want {
			t.Fatal(listen, got, err)
		}
	}
	for _, config := range []struct {
		remote, anonymous bool
		api, runtime      string
	}{
		{false, true, "", strings.Repeat("r", 32)}, {true, true, "", ""}, {true, false, "", strings.Repeat("r", 32)}, {true, false, strings.Repeat("r", 32), strings.Repeat("r", 32)},
	} {
		if _, err := ControlURL("0.0.0.0:17343", config.remote, config.anonymous, config.api, config.runtime); err == nil {
			t.Fatal("unsafe launch accepted")
		}
	}
}
