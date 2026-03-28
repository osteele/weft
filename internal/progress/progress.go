package progress

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ProgressSource indicates how a progress value was detected.
type ProgressSource int

const (
	SourceExplicit ProgressSource = iota // "Progress:" prefix
	SourceTqdm                           // tqdm bar (NN%|)
	SourceEpoch                          // Epoch N/M fallback
)

// Progress represents parsed progress information from a job
type Progress struct {
	Percent int            // 0-100, -1 if using Current/Total instead
	Current int            // For "N/M" format (0 if using Percent)
	Total   int            // For "N/M" format (0 if using Percent)
	RawLine string         // The original progress line
	Source  ProgressSource // How this progress was detected
}

// DisplayPercent returns the progress as a percentage (0-100).
// For Current/Total format, it calculates the percentage.
// Returns -1 if progress cannot be determined.
func (p *Progress) DisplayPercent() int {
	if p.Percent >= 0 {
		return p.Percent
	}
	if p.Total > 0 {
		return (p.Current * 100) / p.Total
	}
	return -1
}

// Tracker tracks file sizes to enable incremental reading
type Tracker struct {
	mu    sync.Mutex
	sizes map[string]int64 // logPath -> last known file size
}

// NewTracker creates a new file size tracker
func NewTracker() *Tracker {
	return &Tracker{
		sizes: make(map[string]int64),
	}
}

// GetLastSize returns the last known file size for a path, or 0 if unknown
func (t *Tracker) GetLastSize(path string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sizes[path]
}

// SetSize updates the tracked file size for a path
func (t *Tracker) SetSize(path string, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sizes[path] = size
}

// Clear removes a path from tracking (e.g., when job completes)
func (t *Tracker) Clear(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sizes, path)
}

// Progress line patterns:
// - Progress: 75%
// - Progress: 9/14
// - Progress: 9 of 14
// - Progress: 9 out of 14
// - tqdm: 45%|████▌     | 450/1000 [01:23<01:42]
// - Epoch 3/10, Epoch [3/10]
var (
	// Matches "Progress: 75%" or "Progress: 75 %"
	percentPattern = regexp.MustCompile(`(?i)^Progress:\s*(\d+)\s*%`)

	// Matches "Progress: 9/14"
	slashPattern = regexp.MustCompile(`(?i)^Progress:\s*(\d+)\s*/\s*(\d+)`)

	// Matches "Progress: 9 of 14" or "Progress: 9 out of 14"
	ofPattern = regexp.MustCompile(`(?i)^Progress:\s*(\d+)\s+(?:of|out of)\s+(\d+)`)

	// Matches tqdm progress bars: "45%|████▌     |"
	tqdmPattern = regexp.MustCompile(`(\d+)%\|`)

	// Matches "Epoch 3/10", "Epoch [3/10]", "Epoch 3/10:", etc.
	epochPattern = regexp.MustCompile(`(?i)Epoch\s*\[?(\d+)\s*/\s*(\d+)\]?`)
)

// ParseProgress parses a single line for progress information.
// Returns nil if no progress pattern found.
func ParseProgress(line string) *Progress {
	line = strings.TrimSpace(line)
	if idx := strings.LastIndex(line, "\r"); idx >= 0 {
		line = strings.TrimSpace(line[idx+1:])
	}

	// Quick pre-filter: skip lines that can't match any pattern.
	lower := strings.ToLower(line)
	hasProgress := strings.Contains(lower, "progress:")
	hasTqdm := strings.Contains(line, "%|")
	hasEpoch := strings.Contains(lower, "epoch")
	if !hasProgress && !hasTqdm && !hasEpoch {
		return nil
	}

	// "Progress:" patterns take priority — extract the Progress: portion.
	if hasProgress {
		if idx := strings.LastIndex(lower, "progress:"); idx >= 0 {
			line = strings.TrimSpace(line[idx:])
		}

		if m := percentPattern.FindStringSubmatch(line); m != nil {
			percent, _ := strconv.Atoi(m[1])
			return &Progress{Percent: percent, RawLine: line, Source: SourceExplicit}
		}
		if m := slashPattern.FindStringSubmatch(line); m != nil {
			current, _ := strconv.Atoi(m[1])
			total, _ := strconv.Atoi(m[2])
			return &Progress{Percent: -1, Current: current, Total: total, RawLine: line, Source: SourceExplicit}
		}
		if m := ofPattern.FindStringSubmatch(line); m != nil {
			current, _ := strconv.Atoi(m[1])
			total, _ := strconv.Atoi(m[2])
			return &Progress{Percent: -1, Current: current, Total: total, RawLine: line, Source: SourceExplicit}
		}
	}

	// tqdm: prefer over epoch since it gives a direct percentage.
	if hasTqdm {
		if m := tqdmPattern.FindStringSubmatch(line); m != nil {
			percent, _ := strconv.Atoi(m[1])
			return &Progress{Percent: percent, RawLine: line, Source: SourceTqdm}
		}
	}

	// Epoch N/M fallback.
	if hasEpoch {
		if m := epochPattern.FindStringSubmatch(line); m != nil {
			current, _ := strconv.Atoi(m[1])
			total, _ := strconv.Atoi(m[2])
			return &Progress{Percent: -1, Current: current, Total: total, RawLine: line, Source: SourceEpoch}
		}
	}

	return nil
}

// GrepCommand returns a shell command that greps for progress lines in logFile.
// Used by TUI and web server to fetch progress via SSH. Returns multiple lines
// so callers can use FindLastProgressPreferExplicit to pick the right one.
func GrepCommand(logFile string) string {
	return fmt.Sprintf("grep -iE 'Progress:|%%\\||Epoch [0-9\\[]' %s 2>/dev/null | tail -20", logFile)
}

// FindLastProgressPreferExplicit searches content for the last progress line,
// preferring explicit "Progress:" lines over tqdm/epoch. If any explicit
// Progress: line exists in the content, tqdm and epoch lines are ignored.
// This prevents spurious phase resets from tqdm bars (e.g. dataset loading)
// when the job also emits explicit Progress: lines for its main work.
func FindLastProgressPreferExplicit(content string) *Progress {
	lines := strings.Split(content, "\n")

	var lastExplicit, lastAny *Progress
	for i := len(lines) - 1; i >= 0; i-- {
		prog := ParseProgress(lines[i])
		if prog == nil {
			continue
		}
		if lastAny == nil {
			lastAny = prog
		}
		if prog.Source == SourceExplicit {
			lastExplicit = prog
			break // scanning from end, so this is the most recent explicit line
		}
	}
	if lastExplicit != nil {
		return lastExplicit
	}
	return lastAny
}

// PhaseTracker detects multi-phase restarts in raw progress output.
// When progress drops by more than 50 points, it increments the phase counter.
// The agent uses this to report phase number + raw percent to R2; the display
// layer is responsible for Bayesian estimation of total phases.
type PhaseTracker struct {
	lastRawPct int
	phase      int // 1-based phase number
}

// NewPhaseTracker creates a tracker starting at phase 1.
func NewPhaseTracker() *PhaseTracker {
	return &PhaseTracker{lastRawPct: -1, phase: 1}
}

// Update records a new raw progress value and detects phase restarts.
// Returns the current (phase, rawPct) pair.
func (pt *PhaseTracker) Update(rawPct int) (phase int, pct int) {
	if rawPct < 0 {
		return pt.phase, rawPct
	}
	if pt.lastRawPct >= 0 && rawPct < pt.lastRawPct-50 {
		pt.phase++
	}
	pt.lastRawPct = rawPct
	return pt.phase, rawPct
}

// EstimateTotalPhases returns E[N | N > k] under a truncated Poisson prior.
// The prior on total phase count is P(N=n) = e^{-λ} λ^{n-1}/(n-1)! (i.e.,
// N-1 ~ Poisson(λ)). After observing k completed phases, we condition on
// N > k and compute the posterior mean from partial sums of the Poisson PMF.
// Returns 1 when phase == 1 (no restarts observed).
func EstimateTotalPhases(phase int, lambda float64) float64 {
	k := phase - 1 // number of completed phases
	if k <= 0 {
		return 1
	}
	// E[N | N > k] = Σ_{n>k} n·w(n) / Σ_{n>k} w(n)  where w(n) = λ^{n-1}/(n-1)!
	var sumW, sumNW float64
	logW := 0.0 // log(w(1)) = 0
	for n := 1; n <= k+200; n++ {
		if n > k {
			w := math.Exp(logW)
			sumW += w
			sumNW += float64(n) * w
		}
		logW += math.Log(lambda) - math.Log(float64(n))
		if n > k && math.Exp(logW) < sumW*1e-12 {
			break
		}
	}
	if sumW == 0 {
		return float64(k + 1)
	}
	return sumNW / sumW
}

// FormatPhaseProgress returns a display string for progress, using "≈" prefix
// when the value is a Bayesian estimate across multiple phases.
// Returns empty string if rawPct <= 0.
func FormatPhaseProgress(phase, rawPct int) string {
	if rawPct <= 0 {
		return ""
	}
	adjusted, isEstimate := PhaseProgress(phase, rawPct, DefaultPoissonLambda)
	if isEstimate {
		return fmt.Sprintf("≈%d%%", adjusted)
	}
	return fmt.Sprintf("%3d%%", adjusted)
}

// DefaultPoissonLambda is the default Poisson prior parameter for phase count
// estimation. λ=2 gives a prior mean of ~3 phases (N-1 ~ Poisson(λ)).
const DefaultPoissonLambda = 2.0

// PhaseProgress computes an adjusted progress percentage from a phase number
// and raw percentage, using a truncated Poisson prior with the given λ.
// Returns (adjustedPct, isEstimate). When phase <= 1, returns raw pct unchanged.
func PhaseProgress(phase, rawPct int, lambda float64) (pct int, isEstimate bool) {
	if phase <= 1 {
		return rawPct, false
	}
	k := phase - 1 // completed phases
	estTotal := EstimateTotalPhases(phase, lambda)
	adjusted := int(math.Round((float64(k)*100 + float64(rawPct)) / estTotal))
	if adjusted > 99 {
		adjusted = 99
	}
	return adjusted, true
}
