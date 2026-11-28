package workerinstall

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

type Service struct {
	Platform     Platform
	Manager      string
	Launcher     string
	Prefix       string
	Root         string
	Main         string
	Installation string
}

// ServiceTemplate renders only. The operator explicitly chooses a user service
// manager and installs/starts the result; reconnect cannot perform those acts.
func ServiceTemplate(s Service) ([]byte, error) {
	for _, p := range []string{s.Launcher, s.Prefix, s.Root} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.IndexFunc(p, unicode.IsControl) >= 0 {
			return nil, errors.New("worker install: service paths must be explicit absolute paths without control characters")
		}
	}
	for _, id := range []string{s.Main, s.Installation} {
		if !releaseID.MatchString(id) || strings.Contains(id, ".") {
			return nil, errors.New("worker install: invalid service installation identity")
		}
	}
	args := []string{s.Launcher, "worker", "run-selected", "--prefix", s.Prefix, "--root", s.Root, "--main", s.Main, "--installation", s.Installation}
	switch {
	case s.Manager == "systemd-user" && s.Platform.OS == "linux":
		quoted := make([]string, len(args))
		for i, arg := range args {
			quoted[i] = strconv.Quote(strings.ReplaceAll(strings.ReplaceAll(arg, "%", "%%"), "$", "$$"))
		}
		return []byte("[Unit]\nDescription=hataori worker resident\n\n[Service]\nType=simple\nExecStart=" + strings.Join(quoted, " ") + "\nRestart=on-failure\nRestartSec=5\nKillMode=process\nUMask=0077\n\n[Install]\nWantedBy=default.target\n"), nil
	case s.Manager == "launch-agent" && s.Platform.OS == "darwin":
		var b bytes.Buffer
		b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>org.hataori.worker.")
		xml.EscapeText(&b, []byte(s.Installation))
		b.WriteString("</string>\n<key>ProgramArguments</key><array>\n")
		for _, arg := range args {
			b.WriteString("<string>")
			xml.EscapeText(&b, []byte(arg))
			b.WriteString("</string>\n")
		}
		b.WriteString("</array>\n<key>LimitLoadToSessionType</key><string>Aqua</string>\n<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>\n<key>AbandonProcessGroup</key><true/>\n<key>Umask</key><integer>63</integer>\n</dict></plist>\n")
		return b.Bytes(), nil
	default:
		return nil, fmt.Errorf("worker install: service manager %q is not supported on %s", s.Manager, s.Platform.OS)
	}
}
