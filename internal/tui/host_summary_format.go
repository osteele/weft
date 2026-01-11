package tui

// hostSummaryFormat represents the display format for host summary metrics
type hostSummaryFormat int

const (
	hostSummaryFull   hostSummaryFormat = iota // "CPU 45%" - full labels
	hostSummaryAbbrev                          // "C45%" - abbreviated labels
)

// Width estimates for host summary segments (status + name + metrics + spaces)
const (
	hostSummaryFullWidth   = 35 // "● hostname   CPU 45% RAM 58% GPU 72%"
	hostSummaryAbbrevWidth = 23 // "● hostname   C45% R58% G72%"
)
