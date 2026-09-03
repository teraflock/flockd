//go:build linux

package memory

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Linux: Pss from /proc/<pid>/smaps_rollup (kernel 4.14+). Proportional set
// size apportions pages shared between processes (two llama-servers on the
// same GGUF each count half), so the sum over runtimes is what the models
// actually occupy; a single mapper's clean file-backed pages still count,
// as they do in macOS's footprint.
func processFootprintBytes(pid int) (uint64, error) {
	return pssFromSmapsRollup(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
}

func pssFromSmapsRollup(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("memory: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Pss:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			break
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("memory: parse Pss: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("memory: no Pss line in %s", path)
}
