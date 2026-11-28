package workerinstall

import (
	"debug/elf"
	"debug/macho"
	"errors"
	"os"
)

// checkBinary inspects the authenticated bytes without ever executing them.
func checkBinary(f *os.File, p Platform) error {
	switch p.OS {
	case "linux":
		e, err := elf.NewFile(f)
		if err == nil && e.Class == elf.ELFCLASS64 && e.Data == elf.ELFDATA2LSB && (e.Type == elf.ET_EXEC || e.Type == elf.ET_DYN) && len(e.Progs) > 0 {
			if p.Arch == "amd64" && e.Machine == elf.EM_X86_64 || p.Arch == "arm64" && e.Machine == elf.EM_AARCH64 {
				return nil
			}
		}
	case "darwin":
		m, err := macho.NewFile(f)
		if err == nil && m.Type == macho.TypeExec && len(m.Loads) > 0 {
			if p.Arch == "amd64" && m.Cpu == macho.CpuAmd64 || p.Arch == "arm64" && m.Cpu == macho.CpuArm64 {
				return nil
			}
		}
	}
	return errors.New("worker install: executable does not match reported worker platform")
}
