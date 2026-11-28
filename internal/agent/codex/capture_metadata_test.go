package codex

import (
	"os"
	"os/exec"
	"testing"
)

func TestSupervisorCaptureMetadata(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to check capture sanitizers")
	}
	cmd := exec.Command(python, "testdata/capture_metadata_test.py")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("capture sanitizer regression: %v\n%s", err, out)
	}
}
