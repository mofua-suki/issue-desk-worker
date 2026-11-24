package hostload

import (
	"context"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const gpuWait = 100 * time.Millisecond
const gpuReadTimeout = 3 * time.Second

// gpuSampler allows only one outstanding read, including a reader that ignores cancellation.
type gpuSampler struct {
	mu      sync.Mutex
	read    func(context.Context) ([]byte, error)
	pending *gpuRequest
}

type gpuRequest struct {
	done   chan struct{}
	result map[string]float64
}

func readGPU(ctx context.Context) ([]byte, error) {
	raw, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=utilization.gpu,memory.free", "--format=csv,noheader,nounits").Output()
	if err == nil && len(parseGPU(raw)) > 0 {
		return raw, nil
	}
	return readDRMGPU("/sys/class/drm")
}

func (s *gpuSampler) measure() map[string]float64 {
	s.mu.Lock()
	if s.pending == nil {
		request := &gpuRequest{done: make(chan struct{})}
		s.pending = request
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), gpuReadTimeout)
			defer cancel()
			raw, err := s.read(ctx)
			var values map[string]float64
			if err == nil {
				values = parseGPU(raw)
			}
			s.mu.Lock()
			request.result = values
			close(request.done)
			s.mu.Unlock()
		}()
	}
	request := s.pending
	s.mu.Unlock()
	timer := time.NewTimer(gpuWait)
	defer timer.Stop()
	select {
	case <-request.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pending == request {
			s.pending = nil
			return request.result
		}
		return nil
	case <-timer.C:
		return nil
	}
}

// A host reports the first GPU; unsupported fields are independent and omitted.
func parseGPU(raw []byte) map[string]float64 {
	line := strings.SplitN(string(raw), "\n", 2)[0]
	fields := strings.Split(line, ",")
	if len(fields) != 2 {
		return nil
	}
	out := make(map[string]float64)
	for i, key := range []string{"gpuPercent", "freeVramMB"} {
		v, err := strconv.ParseFloat(strings.TrimSpace(fields[i]), 64)
		if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && (i != 0 || v <= 100) {
			out[key] = v
		}
	}
	return out
}
