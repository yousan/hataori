// ci-feedback is executed only on an ephemeral CI runner. Its source/module
// closure must match the approved control before feedback can be admitted.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/yousan/hataori/cmd/internal/cipolicy"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "immutable base and target required")
		os.Exit(2)
	}
	plan := cipolicy.Select(".", os.Args[1], os.Args[2])
	json.NewEncoder(os.Stdout).Encode(plan)
	if plan.Full || plan.HighRisk {
		fmt.Fprintln(os.Stderr, "full ready-head route required")
		os.Exit(1)
	}
	if plan.DocsOnly {
		return
	}
	packages := append(append([]string{}, plan.Packages...), plan.Smoke...)
	if len(packages) == 0 {
		os.Exit(1)
	}
	command := exec.Command("go", append([]string{"test", "-count=1", "-timeout", "10m"}, packages...)...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		os.Exit(1)
	}
}
