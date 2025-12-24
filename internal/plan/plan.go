package plan

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// File represents a parsed job plan file
type File struct {
	Version int64   `yaml:"version"`
	Kill    []int64 `yaml:"kill"`
	Jobs    []Entry `yaml:"jobs"`
}

// Defaults contains values that can be applied to a parsed plan.
type Defaults struct {
	Host string
}

// Entry represents one item in the plan jobs list
type Entry struct {
	Job      *Job      `yaml:"job"`
	Parallel *Parallel `yaml:"parallel"`
	Series   *Series   `yaml:"series"`
}

// Job represents a single job specification
type Job struct {
	Name        string            `yaml:"name"`
	Host        string            `yaml:"host"`
	Dir         string            `yaml:"dir"`
	Command     string            `yaml:"command"`
	Description string            `yaml:"description"`
	Env         map[string]string `yaml:"env"`
	Queue       string            `yaml:"queue"`
	QueueOnly   bool              `yaml:"queue_only"`
	When        *When             `yaml:"when"`
}

// Parallel represents a block of jobs that can start at the same time
type Parallel struct {
	Name string            `yaml:"name"`
	Dir  string            `yaml:"dir"`
	Env  map[string]string `yaml:"env"`
	Jobs []Job             `yaml:"jobs"`
}

// Series represents a block of jobs that should run sequentially
type Series struct {
	Name  string            `yaml:"name"`
	Dir   string            `yaml:"dir"`
	Env   map[string]string `yaml:"env"`
	Queue string            `yaml:"queue"`
	Wait  string            `yaml:"wait"`
	Jobs  []Job             `yaml:"jobs"`
}

// When represents the reserved future syntax for resource triggers
type When struct {
	CPUBelow  *float64 `yaml:"cpu_below"`
	RAMFreeGB *float64 `yaml:"ram_free_gb"`
	GPU       *WhenGPU `yaml:"gpu"`
}

// WhenGPU captures potential future GPU constraints
type WhenGPU struct {
	Device       any      `yaml:"device"`
	Utilization  *float64 `yaml:"util_below"`
	MemoryFreeGB *float64 `yaml:"memory_free_gb"`
}

// Decode parses the YAML data into a plan File
func Decode(data []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate ensures the plan file contains supported constructs
func (f *File) Validate() error {
	if f.Version != 1 {
		if f.Version == 0 {
			return fmt.Errorf("plan file missing required version: set version: 1")
		}
		return fmt.Errorf("unsupported plan version %d", f.Version)
	}
	if len(f.Jobs) == 0 {
		return fmt.Errorf("plan must contain at least one job entry")
	}
	for i, entry := range f.Jobs {
		if err := entry.validate(fmt.Sprintf("jobs[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDefaults fills in missing values such as host names.
func (f *File) ApplyDefaults(defaults Defaults) error {
	for i := range f.Jobs {
		path := fmt.Sprintf("jobs[%d]", i)
		if err := f.Jobs[i].applyDefaults(defaults, path); err != nil {
			return err
		}
	}
	return nil
}

func (e *Entry) validate(path string) error {
	count := 0
	if e.Job != nil {
		count++
		if err := e.Job.validate(path + ".job"); err != nil {
			return err
		}
	}
	if e.Parallel != nil {
		count++
		if err := e.Parallel.validate(path + ".parallel"); err != nil {
			return err
		}
	}
	if e.Series != nil {
		count++
		if err := e.Series.validate(path + ".series"); err != nil {
			return err
		}
	}
	if count == 0 {
		return fmt.Errorf("%s must contain job, parallel, or series", path)
	}
	if count > 1 {
		return fmt.Errorf("%s cannot contain more than one of job/parallel/series", path)
	}
	return nil
}

func (e *Entry) applyDefaults(defaults Defaults, path string) error {
	if e.Job != nil {
		return e.Job.applyDefaults(defaults, path+".job")
	}
	if e.Parallel != nil {
		return e.Parallel.applyDefaults(defaults, path+".parallel")
	}
	if e.Series != nil {
		return e.Series.applyDefaults(defaults, path+".series")
	}
	return nil
}

func (j *Job) validate(path string) error {
	if j.Command == "" {
		return fmt.Errorf("%s missing command", path)
	}
	if j.Host == "" {
		return fmt.Errorf("%s missing host", path)
	}
	if j.When != nil {
		return fmt.Errorf("%s: the when block is not supported in this version", path)
	}
	return nil
}

func (j *Job) applyDefaults(defaults Defaults, path string) error {
	if j.Host == "" {
		if defaults.Host == "" {
			return fmt.Errorf("%s.host missing (provide --host or set host in the plan)", path)
		}
		j.Host = defaults.Host
	}
	return nil
}

func (p *Parallel) validate(path string) error {
	if len(p.Jobs) == 0 {
		return fmt.Errorf("%s must contain at least one job", path)
	}
	for i := range p.Jobs {
		jobPath := fmt.Sprintf("%s.jobs[%d]", path, i)
		if err := p.Jobs[i].validate(jobPath); err != nil {
			return err
		}
	}
	return nil
}

func (p *Parallel) applyDefaults(defaults Defaults, path string) error {
	for i := range p.Jobs {
		jobPath := fmt.Sprintf("%s.jobs[%d]", path, i)
		if err := p.Jobs[i].applyDefaults(defaults, jobPath); err != nil {
			return err
		}
	}
	return nil
}

func (s *Series) validate(path string) error {
	if len(s.Jobs) == 0 {
		return fmt.Errorf("%s must contain at least one job", path)
	}
	for i := range s.Jobs {
		jobPath := fmt.Sprintf("%s.jobs[%d]", path, i)
		if err := s.Jobs[i].validate(jobPath); err != nil {
			return err
		}
	}
	switch s.Wait {
	case "", "success", "any":
	default:
		return fmt.Errorf("%s.wait must be 'success' or 'any'", path)
	}
	return nil
}

func (s *Series) applyDefaults(defaults Defaults, path string) error {
	for i := range s.Jobs {
		jobPath := fmt.Sprintf("%s.jobs[%d]", path, i)
		if err := s.Jobs[i].applyDefaults(defaults, jobPath); err != nil {
			return err
		}
	}
	return nil
}
