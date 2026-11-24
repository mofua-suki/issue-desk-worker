package hostload

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readDRMGPU reads AMD's utilization and dedicated VRAM counters when exposed.
func readDRMGPU(root string) ([]byte, error) {
	cards, err := filepath.Glob(filepath.Join(root, "card[0-9]*"))
	if err != nil {
		return nil, err
	}
	for _, card := range cards {
		// Connector names also match the glob; only card numbers are devices.
		if _, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(card), "card")); err != nil {
			continue
		}
		device := filepath.Join(card, "device")
		vendor, err := os.ReadFile(filepath.Join(device, "vendor"))
		if err != nil || strings.TrimSpace(string(vendor)) != "0x1002" {
			continue
		}
		busy, err := os.ReadFile(filepath.Join(device, "gpu_busy_percent"))
		if err != nil {
			continue
		}
		utilization := strings.TrimSpace(string(busy))
		if len(parseGPU([]byte(utilization+", N/A"))) == 0 {
			continue
		}
		free := "N/A"
		totalRaw, e1 := os.ReadFile(filepath.Join(device, "mem_info_vram_total"))
		usedRaw, e2 := os.ReadFile(filepath.Join(device, "mem_info_vram_used"))
		total, e3 := strconv.ParseUint(strings.TrimSpace(string(totalRaw)), 10, 64)
		used, e4 := strconv.ParseUint(strings.TrimSpace(string(usedRaw)), 10, 64)
		if e1 == nil && e2 == nil && e3 == nil && e4 == nil && total > 0 && used <= total {
			free = strconv.FormatFloat(float64(total-used)/(1024*1024), 'f', -1, 64)
		}
		return []byte(fmt.Sprintf("%s, %s", utilization, free)), nil
	}
	return nil, errors.New("GPU counters unavailable")
}
