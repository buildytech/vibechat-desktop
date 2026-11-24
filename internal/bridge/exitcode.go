package bridge

import (
	"fmt"
	"os"
	"strings"
)

// ntstatus are the Windows process exit codes of a crash (NTSTATUS), with what they mean for the bridge.
var ntstatus = map[uint32]string{
	0xC000001D: "STATUS_ILLEGAL_INSTRUCTION: CPU не поддерживает инструкции рантайма bridge (AVX/AVX2), нужна baseline-сборка",
	0xC0000005: "STATUS_ACCESS_VIOLATION: the bridge crashed (memory access); antivirus injection or a damaged file",
	0xC0000135: "STATUS_DLL_NOT_FOUND: a system DLL the bridge needs is missing",
	0xC0000139: "STATUS_ENTRYPOINT_NOT_FOUND: Windows is too old for the bridge runtime",
	0xC0000142: "STATUS_DLL_INIT_FAILED: a DLL failed to initialize (desktop heap or antivirus)",
	0xC00000FD: "STATUS_STACK_OVERFLOW: the bridge overflowed its stack",
	0xC0000409: "STATUS_STACK_BUFFER_OVERRUN: the bridge aborted (fail-fast)",
	0xC0000022: "STATUS_ACCESS_DENIED: the bridge executable may not run (antivirus or permissions)",
	0xC000007B: "STATUS_INVALID_IMAGE_FORMAT: the bridge executable is damaged or for another CPU",
}

// DescribeExit renders a process exit: the code, in hex for a Windows NTSTATUS, and what it means.
func DescribeExit(st *os.ProcessState) string {
	if st == nil {
		return "unknown"
	}
	code := st.ExitCode()
	if code == -1 { // killed by a signal (Unix)
		s := st.String()
		if strings.Contains(s, "illegal instruction") {
			return s + " — CPU не поддерживает инструкции рантайма bridge (AVX/AVX2), нужна baseline-сборка"
		}
		return s
	}
	return describeCode(code)
}

func describeCode(code int) string {
	u := uint32(code)
	if hint, ok := ntstatus[u]; ok {
		return fmt.Sprintf("%d = 0x%08X %s", code, u, hint)
	}
	if u >= 0xC0000000 {
		return fmt.Sprintf("%d = 0x%08X (NTSTATUS error)", code, u)
	}
	return fmt.Sprint(code)
}
