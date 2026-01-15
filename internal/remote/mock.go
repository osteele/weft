package remote

// MockProber is a test implementation of Prober.
type MockProber struct {
	CompletedResult ProbeResult
	CompletionInfo  *CompletionInfo
	CurrentResult   ProbeResult
	InQueueResult   ProbeResult
	ProcessResult   ProbeResult
}

func (m *MockProber) ProbeInQueue(queueName string, jobID int64) ProbeResult {
	return m.InQueueResult
}

func (m *MockProber) ProbeCurrent(queueName string, jobID int64) ProbeResult {
	return m.CurrentResult
}

func (m *MockProber) ProbeCompleted(jobID int64) (ProbeResult, *CompletionInfo) {
	return m.CompletedResult, m.CompletionInfo
}

func (m *MockProber) ProbeProcessRunning(jobID int64) ProbeResult {
	return m.ProcessResult
}

// MockHost is a test implementation of Host.
type MockHost struct {
	InQueueResult    bool
	InQueueErr       error
	CurrentResult    bool
	CurrentErr       error
	CompletionResult *CompletionInfo
	CompletionErr    error
	ProcessResult    bool
	ProcessErr       error
	MetadataResult   map[string]string
	MetadataErr      error
	SamplesResult    string
	SamplesErr       error
	TmuxExistsResult bool
	TmuxExistsErr    error
	AppendCalls      []QueueEntry
	RemoveCalls      []int64
}

func (m *MockHost) IsJobInQueue(queueName string, jobID int64) (bool, error) {
	return m.InQueueResult, m.InQueueErr
}

func (m *MockHost) IsJobCurrent(queueName string, jobID int64) (bool, error) {
	return m.CurrentResult, m.CurrentErr
}

func (m *MockHost) AppendToQueue(queueName string, entry QueueEntry) error {
	m.AppendCalls = append(m.AppendCalls, entry)
	return nil
}

func (m *MockHost) RemoveFromQueue(queueName string, jobID int64) error {
	m.RemoveCalls = append(m.RemoveCalls, jobID)
	return nil
}

func (m *MockHost) GetJobCompletion(jobID int64) (*CompletionInfo, error) {
	return m.CompletionResult, m.CompletionErr
}

func (m *MockHost) IsProcessRunning(jobID int64) (bool, error) {
	return m.ProcessResult, m.ProcessErr
}

func (m *MockHost) GetJobMetadata(jobID int64) (map[string]string, error) {
	return m.MetadataResult, m.MetadataErr
}

func (m *MockHost) GetJobSamples(jobID int64) (string, error) {
	return m.SamplesResult, m.SamplesErr
}

func (m *MockHost) TmuxSessionExists(sessionName string) (bool, error) {
	return m.TmuxExistsResult, m.TmuxExistsErr
}

func (m *MockHost) StartTmuxSession(sessionName, command string) error {
	return nil
}

func (m *MockHost) KillTmuxSession(sessionName string) error {
	return nil
}
