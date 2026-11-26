package main

// Baseline x64 rebuild of the bridge. The upstream cursor-sdk-bridge standalone builds are `bun build --compile`
// executables on Bun's default x64 runtime, which needs AVX2: on a CPU without it the bridge dies at once with
// SIGILL (Linux) or 0xC000001D STATUS_ILLEGAL_INSTRUCTION (Windows) before printing anything. For x64 we take the
// bundled JavaScript out of the pinned upstream binary (one module, no bytecode) and compile it again with the
// pinned Bun *baseline* runtime of the same version (Nehalem level, no AVX), with the same autoload flags. The
// result is checked: its machine code (.text / __text) must equal the pinned baseline runtime's.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// bunTrailer ends the payload of a Bun standalone executable: [payload][offsets][trailer].
var bunTrailer = []byte("\n---- Bun! ----\n")

const (
	offsetsSize = 32 // byte_count u64, modules StringPointer, entry_point_id u32, exec_argv StringPointer, flags u32
	moduleSize  = 52 // name, contents, sourcemap, bytecode, module_info, bytecode_origin StringPointers + 4 bytes
)

// bunTarget maps a bridge platform (node naming) to Bun's: win32-x64 -> windows-x64.
func bunTarget(plat string) string { return strings.Replace(plat, "win32-", "windows-", 1) }

func exeName(plat string) string {
	if strings.HasPrefix(plat, "win32-") {
		return "cursor-sdk-bridge.exe"
	}
	return "cursor-sdk-bridge"
}

// extractBundle returns the single JavaScript module embedded in a Bun standalone executable.
func extractBundle(bin []byte) (name string, js []byte, err error) {
	t := bytes.LastIndex(bin, bunTrailer)
	if t < offsetsSize {
		return "", nil, errors.New("not a Bun standalone executable (no trailer)")
	}
	off := bin[t-offsetsSize : t]
	byteCount := binary.LittleEndian.Uint64(off[0:8])
	modOff, modLen := binary.LittleEndian.Uint32(off[8:12]), binary.LittleEndian.Uint32(off[12:16])
	if byteCount > uint64(t-offsetsSize) {
		return "", nil, errors.New("bad payload size")
	}
	payload := bin[t-offsetsSize-int(byteCount) : t-offsetsSize]
	ptr := func(rec []byte, i int) ([]byte, error) {
		o, l := binary.LittleEndian.Uint32(rec[i*8:]), binary.LittleEndian.Uint32(rec[i*8+4:])
		if uint64(o)+uint64(l) > uint64(len(payload)) {
			return nil, errors.New("pointer out of payload")
		}
		return payload[o : o+l], nil
	}
	if uint64(modOff)+uint64(modLen) > uint64(len(payload)) || modLen != moduleSize {
		return "", nil, fmt.Errorf("want exactly one embedded module, module table is %d bytes", modLen)
	}
	rec := payload[modOff : modOff+modLen]
	n, err := ptr(rec, 0)
	if err != nil {
		return "", nil, err
	}
	js, err = ptr(rec, 1)
	if err != nil {
		return "", nil, err
	}
	for i := 2; i < 6; i++ { // sourcemap, bytecode, module info, bytecode origin: must be empty to rebuild from JS
		if p, err := ptr(rec, i); err != nil || len(p) != 0 {
			return "", nil, fmt.Errorf("embedded module has extra data (field %d): cannot rebuild from source", i)
		}
	}
	if !bytes.HasPrefix(js, []byte("// @bun\n")) {
		return "", nil, errors.New("embedded module is not Bun-bundled JavaScript")
	}
	return string(n), js, nil
}

// codeSection returns the main machine-code section of an ELF, PE or Mach-O executable.
func codeSection(b []byte) ([]byte, error) {
	r := bytes.NewReader(b)
	if f, err := elf.NewFile(r); err == nil {
		if s := f.Section(".text"); s != nil {
			return s.Data()
		}
		return nil, errors.New("ELF without .text")
	}
	if f, err := pe.NewFile(r); err == nil {
		if s := f.Section(".text"); s != nil {
			d, err := s.Data()
			if err != nil {
				return nil, err
			}
			if s.VirtualSize > 0 && int(s.VirtualSize) < len(d) {
				d = d[:s.VirtualSize] // raw data is padded to the file alignment
			}
			return d, nil
		}
		return nil, errors.New("PE without .text")
	}
	if f, err := macho.NewFile(r); err == nil {
		if s := f.Section("__text"); s != nil {
			return s.Data()
		}
		return nil, errors.New("Mach-O without __text")
	}
	return nil, errors.New("unknown executable format")
}

// bunRuntime downloads (cached) and verifies the pinned baseline Bun for plat and returns its executable bytes.
func bunRuntime(l lock, plat, cache string) ([]byte, error) {
	sum, ok := l.Baseline.SHA256[plat]
	if !ok {
		return nil, fmt.Errorf("no baseline Bun pinned for %s", plat)
	}
	url := strings.ReplaceAll(l.Baseline.URL, "{target}", bunTarget(plat))
	z := fetch(url, cache)
	if got := sha256.Sum256(z); hex.EncodeToString(got[:]) != sum {
		return nil, fmt.Errorf("bun %s: sha256 %x, want %s", plat, got, sum)
	}
	zr, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if base := filepath.Base(f.Name); base == "bun" || base == "bun.exe" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%s: no bun executable inside", url)
}

func needsBaseline(l lock, plat string) bool {
	for _, p := range l.Baseline.Platforms {
		if p == plat {
			return true
		}
	}
	return false
}

// rebuildBaseline replaces <out>/bin/<bridge> with the same bundle compiled on the baseline runtime.
func rebuildBaseline(l lock, plat, out, cache string) error {
	manifestPath := filepath.Join(out, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if want := "bun-" + l.Baseline.Bun; m["runtime"] != want {
		return fmt.Errorf("upstream bridge runtime is %v, the pinned baseline Bun is %s: bump bridge.lock.json together", m["runtime"], want)
	}
	exe := filepath.Join(out, "bin", exeName(plat))
	orig, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	name, js, err := extractBundle(orig)
	if err != nil {
		return fmt.Errorf("%s: %w", exe, err)
	}
	if !strings.HasSuffix(name, "/"+exeName(plat)) {
		return fmt.Errorf("%s: unexpected embedded module %q", exe, name)
	}
	hostPlat := platform()
	if !strings.HasSuffix(hostPlat, "-x64") {
		return fmt.Errorf("baseline rebuild runs the x64 baseline Bun and needs an x64 host, this is %s", hostPlat)
	}
	work, err := os.MkdirTemp("", "fetch-bridge-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	writeExe := func(p string, b []byte) error { return os.WriteFile(p, b, 0o755) }
	hostBun, err := bunRuntime(l, hostPlat, cache)
	if err != nil {
		return err
	}
	compiler := filepath.Join(work, "bun"+map[bool]string{true: ".exe"}[runtime.GOOS == "windows"])
	if err := writeExe(compiler, hostBun); err != nil {
		return err
	}
	targetBun := hostBun
	if plat != hostPlat {
		if targetBun, err = bunRuntime(l, plat, cache); err != nil {
			return err
		}
	}
	targetExe := filepath.Join(work, "target-"+filepath.Base(exe)+".runtime")
	if err := writeExe(targetExe, targetBun); err != nil {
		return err
	}
	src := filepath.Join(work, "cursor-sdk-bridge.js")
	if err := os.WriteFile(src, js, 0o644); err != nil {
		return err
	}
	built := filepath.Join(work, "out", exeName(plat))
	target := "bun-" + bunTarget(plat) + "-baseline"
	cmd := procgroup.Command(compiler, "build", src, "--compile", "--target="+target, "--compile-executable-path="+targetExe,
		// the upstream build disables every runtime autoload (offsets flags 0x0f); keep that
		"--no-compile-autoload-dotenv", "--no-compile-autoload-bunfig",
		"--no-compile-autoload-tsconfig", "--no-compile-autoload-package-json",
		"--outfile", built)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "DO_NOT_TRACK=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bun build %s: %v\n%s", target, err, b)
	}
	nb, err := os.ReadFile(built)
	if err != nil {
		return err
	}
	if err := checkBaseline(nb, targetBun); err != nil {
		return fmt.Errorf("rebuilt bridge: %w", err)
	}
	if err := writeExe(exe, nb); err != nil {
		return err
	}
	jsSum := sha256.Sum256(js)
	m["runtime"] = "bun-" + l.Baseline.Bun + "-baseline"
	m["baselineRebuild"] = map[string]any{
		"target":       target + "-v" + l.Baseline.Bun,
		"bunSha256":    l.Baseline.SHA256[plat],
		"bundleSha256": hex.EncodeToString(jsSum[:]),
		"upstream":     "bun-" + l.Baseline.Bun,
	}
	enc, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(manifestPath, append(enc, '\n'), 0o644)
}

// checkBaseline: bin is a Bun standalone executable with a bundle whose machine code is the baseline runtime's.
func checkBaseline(bin, bunExe []byte) error {
	if _, _, err := extractBundle(bin); err != nil {
		return err
	}
	a, err := codeSection(bin)
	if err != nil {
		return err
	}
	b, err := codeSection(bunExe)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return errors.New("machine code is not the pinned Bun baseline runtime (AVX2 build?)")
	}
	return nil
}

// verifyDir is `fetch-bridge -verify <dir>`: the shipped bridge folder must run on the baseline runtime.
func verifyDir(l lock, dir, cache string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	var m struct {
		OS, Arch, Entrypoint, Runtime string
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	plat := m.OS + "-" + m.Arch
	if !needsBaseline(l, plat) {
		fmt.Printf("fetch-bridge verify: %s is not an x64 baseline platform, runtime %s\n", plat, m.Runtime)
		return nil
	}
	if m.Runtime != "bun-"+l.Baseline.Bun+"-baseline" {
		return fmt.Errorf("%s: manifest runtime %q, want bun-%s-baseline", dir, m.Runtime, l.Baseline.Bun)
	}
	bin, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(m.Entrypoint)))
	if err != nil {
		return err
	}
	bun, err := bunRuntime(l, plat, cache)
	if err != nil {
		return err
	}
	if err := checkBaseline(bin, bun); err != nil {
		return fmt.Errorf("%s: %w", m.Entrypoint, err)
	}
	fmt.Printf("fetch-bridge verify: %s %s = Bun %s baseline runtime (machine code identical)\n", plat, m.Entrypoint, l.Baseline.Bun)
	return nil
}
