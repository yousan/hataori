package workerinstall

import (
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"errors"
	"os"
)

// checkBinary inspects the authenticated bytes without ever executing them.
func checkBinary(f *os.File, p Platform) error {
	info, err := f.Stat()
	if err != nil || info.Size() <= 0 {
		return errors.New("worker install: executable size is unavailable")
	}
	size := uint64(info.Size())
	within := func(offset, length uint64) bool { return offset <= size && length <= size-offset }
	build, err := buildinfo.Read(f)
	if err != nil {
		return errors.New("worker install: executable has no Go build provenance metadata")
	}
	var goos, goarch string
	for _, setting := range build.Settings {
		switch setting.Key {
		case "GOOS":
			goos = setting.Value
		case "GOARCH":
			goarch = setting.Value
		}
	}
	if goos != p.OS || goarch != p.Arch {
		return errors.New("worker install: executable build platform does not match worker platform")
	}
	switch p.OS {
	case "linux":
		e, err := elf.NewFile(f)
		if err == nil && (e.OSABI == elf.ELFOSABI_NONE || e.OSABI == elf.ELFOSABI_LINUX) && e.Class == elf.ELFCLASS64 && e.Data == elf.ELFDATA2LSB && (e.Type == elf.ET_EXEC || e.Type == elf.ET_DYN) && len(e.Progs) > 0 {
			for _, segment := range e.Progs {
				if !within(segment.Off, segment.Filesz) || segment.Type == elf.PT_LOAD && segment.Filesz > segment.Memsz {
					return errors.New("worker install: incomplete ELF segment")
				}
			}
			for _, section := range e.Sections {
				if section.Type != elf.SHT_NOBITS && !within(section.Offset, section.FileSize) {
					return errors.New("worker install: incomplete ELF section")
				}
			}
			if p.Arch == "amd64" && e.Machine == elf.EM_X86_64 || p.Arch == "arm64" && e.Machine == elf.EM_AARCH64 {
				return nil
			}
		}
	case "darwin":
		m, err := macho.NewFile(f)
		if err == nil && m.Type == macho.TypeExec && len(m.Loads) > 0 {
			for _, load := range m.Loads {
				if segment, ok := load.(*macho.Segment); ok && (!within(segment.Offset, segment.Filesz) || segment.Maxprot != 0 && segment.Filesz > segment.Memsz) {
					return errors.New("worker install: incomplete Mach-O segment")
				}
			}
			if p.Arch == "amd64" && m.Cpu == macho.CpuAmd64 || p.Arch == "arm64" && m.Cpu == macho.CpuArm64 {
				return nil
			}
		}
	}
	return errors.New("worker install: executable does not match reported worker platform")
}
