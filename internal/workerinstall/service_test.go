package workerinstall

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestExplicitServiceTemplatesKeepHosts(t *testing.T) {
	s := Service{Platform: Platform{"linux", "amd64"}, Manager: "systemd-user", Launcher: "/fixture/bin/v1", Prefix: "/fixture/versions", Root: "/fixture/worker", Main: "main-A", Installation: "worker-A"}
	b, err := ServiceTemplate(s)
	if err != nil || !bytes.Contains(b, []byte("KillMode=process\n")) || !bytes.Contains(b, []byte(`"worker" "run-selected"`)) || bytes.Contains(b, []byte("ExecStop=")) {
		t.Fatalf("unsafe user-service template: %s %v", b, err)
	}
	s.Root = "/fixture/space %n $HOME"
	b, err = ServiceTemplate(s)
	if err != nil || !bytes.Contains(b, []byte(`"/fixture/space %%n $$HOME"`)) {
		t.Fatalf("unsafe systemd expansion: %s %v", b, err)
	}
	s.Platform.OS = "darwin"
	s.Manager = "launch-agent"
	s.Root = "/fixture/worker<&>"
	b, err = ServiceTemplate(s)
	if err != nil || !bytes.Contains(b, []byte("<key>AbandonProcessGroup</key><true/>")) || !bytes.Contains(b, []byte("<key>LimitLoadToSessionType</key><string>Aqua</string>")) || !bytes.Contains(b, []byte("/fixture/worker&lt;&amp;&gt;")) {
		t.Fatalf("unsafe launch-agent template: %s %v", b, err)
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	for _, manager := range []string{"", "system", "systemd-user", "auto", "launch-agent\n"} {
		s.Manager = manager
		if _, err := ServiceTemplate(s); err == nil {
			t.Fatalf("accepted manager %q on darwin", manager)
		}
	}
	s.Manager = "launch-agent"
	s.Root = "/fixture/worker\n<string>injection</string>"
	if _, err := ServiceTemplate(s); err == nil {
		t.Fatal("accepted newline injection")
	}
	s.Root = "relative"
	if _, err := ServiceTemplate(s); err == nil {
		t.Fatal("accepted relative service root")
	}
	s.Root = "/fixture/worker"
	s.Installation = strings.Repeat("a", 200)
	if _, err := ServiceTemplate(s); err == nil {
		t.Fatal("accepted invalid identity")
	}
}
