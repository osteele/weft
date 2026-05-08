// Package runpod wraps the runpodctl CLI for searching GPU pods,
// creating instances, template management, and provider diagnostics.
package runpod

// Pod represents a RunPod pod from the CLI/API.
type Pod struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"desiredStatus"`
	GPUType     string  `json:"gpuType"`
	GPUCount    int     `json:"gpuCount"`
	CostPerHour float64 `json:"costPerHr"`
	SSHHost     string  `json:"sshHost"`
	SSHPort     int     `json:"sshPort"`
}

// User represents RunPod account information returned by `runpodctl user`.
type User struct {
	ClientBalance    float64 `json:"clientBalance"`
	CurrentSpendHr   float64 `json:"currentSpendPerHr"`
	SpendLimit       float64 `json:"spendLimit"`
	NotifyLowBalance bool    `json:"notifyLowBalance"`
}

// TemplateInfo captures the subset of template fields we care about.
type TemplateInfo struct {
	ID             string
	Name           string
	Image          string
	DockerStartCmd string
	Readme         string
}

// BootstrapTemplateSpec is the desired managed-template configuration.
type BootstrapTemplateSpec struct {
	Name         string
	SpecHash     string
	Image        string
	StartCommand string
	Readme       string
}

// Check is a single readiness check emitted by doctor/setup.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Diagnosis captures RunPod readiness for search and launch workflows.
type Diagnosis struct {
	Enabled               bool
	CLIPath               string
	Version               string
	SearchCommand         string
	PodCommandFamily      string
	TemplateCommandFamily string
	RequiredStartCommand  string
	DefaultImage          string
	TemplateID            string
	Template              *TemplateInfo
	TemplateCompatible    bool
	SearchReady           bool
	LaunchReady           bool
	SearchChecks          []Check
	LaunchChecks          []Check
}

// SetupResult describes what runpod setup changed.
type SetupResult struct {
	Diagnosis       *Diagnosis
	Template        *TemplateInfo
	ConfigPath      string
	CreatedTemplate bool
	UpdatedConfig   bool
}
