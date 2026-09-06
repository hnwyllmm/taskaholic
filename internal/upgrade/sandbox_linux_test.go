package upgrade

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

func TestBubblewrapPrivateFilesystemNetworkAndCandidateBuild(t *testing.T) {
	if os.Getenv("WORK_ASSISTANT_VALIDATION_SANDBOX") != "" {
		t.Skip("already running inside candidate validation")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap not installed")
	}
	root, data := testProject(t)
	secret := filepath.Join(root, "host-only.txt")
	writeTestFile(t, secret, "private canary", 0o600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m, err := New(Config{Root: root, DataDir: data}, fakeAdapter{change: func(directory string) error {
		source := fmt.Sprintf(`package greeting
import("os";"net";"net/http";"net/http/httptest";"testing";"time")
func TestIsolation(t *testing.T) {
 if _,err:=os.ReadFile(%q);err==nil { t.Fatal("read host private file") }
 if err:=os.WriteFile(%q,[]byte("escape"),0600);err==nil { t.Fatal("wrote host file") }
 if err:=os.WriteFile("/tmp/escape",[]byte("escape"),0600);err==nil { t.Fatal("wrote outside job") }
 if connection,err:=net.DialTimeout("tcp",%q,200*time.Millisecond);err==nil { connection.Close();t.Fatal("reached host network") }
 local:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.WriteHeader(200)}));defer local.Close()
 response,err:=http.Get(local.URL);if err!=nil {t.Fatal("private loopback unavailable",err)};response.Body.Close()
 if err:=os.WriteFile(os.Getenv("TMPDIR")+"/inside",[]byte("ok"),0600);err!=nil { t.Fatal(err) }
}
`, secret, filepath.Join(root, "escape"), listener.Addr().String())
		return os.WriteFile(filepath.Join(directory, "internal/greeting/isolation_test.go"), []byte(source), 0o600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckValidationSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := m.Prepare(context.Background(), model.Upgrade{ID: "bubblewrap-fixture", Instructions: "test isolation"})
	if u.State != "READY" {
		t.Fatalf("%s\n%s", u.Error, u.Log)
	}
	if !strings.Contains(u.Log, "[Linux bubblewrap]") {
		t.Fatal("missing sandbox evidence")
	}
	body, _ := os.ReadFile(secret)
	if string(body) != "private canary" {
		t.Fatal("host data changed")
	}
}
