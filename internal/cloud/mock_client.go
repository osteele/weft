package cloud

import "time"

// MockClient is a test double for cloud.Client.
type MockClient struct {
	ProviderVal         Provider
	AvailableFunc       func() error
	SearchOffersFunc    func(OfferConstraints) ([]Offer, error)
	CreateInstanceFunc  func(string, CreateOpts) (*Instance, error)
	ShowInstanceFunc    func(string) (*Instance, error)
	WaitReadyFunc       func(string, time.Duration) (*Instance, error)
	DestroyInstanceFunc func(string) error
	WorkspacePathVal    string
	SelfDestructCmdVal  string
}

var _ Client = (*MockClient)(nil)

func (m *MockClient) Provider() Provider {
	if m.ProviderVal != "" {
		return m.ProviderVal
	}
	return "mock"
}

func (m *MockClient) Available() error {
	if m.AvailableFunc != nil {
		return m.AvailableFunc()
	}
	return nil
}

func (m *MockClient) SearchOffers(c OfferConstraints) ([]Offer, error) {
	if m.SearchOffersFunc != nil {
		return m.SearchOffersFunc(c)
	}
	return nil, nil
}

func (m *MockClient) CreateInstance(offerID string, opts CreateOpts) (*Instance, error) {
	if m.CreateInstanceFunc != nil {
		return m.CreateInstanceFunc(offerID, opts)
	}
	return nil, nil
}

func (m *MockClient) ShowInstance(instanceID string) (*Instance, error) {
	if m.ShowInstanceFunc != nil {
		return m.ShowInstanceFunc(instanceID)
	}
	return nil, nil
}

func (m *MockClient) WaitReady(instanceID string, timeout time.Duration) (*Instance, error) {
	if m.WaitReadyFunc != nil {
		return m.WaitReadyFunc(instanceID, timeout)
	}
	return nil, nil
}

func (m *MockClient) DestroyInstance(instanceID string) error {
	if m.DestroyInstanceFunc != nil {
		return m.DestroyInstanceFunc(instanceID)
	}
	return nil
}

func (m *MockClient) WorkspacePath() string {
	if m.WorkspacePathVal != "" {
		return m.WorkspacePathVal
	}
	return "/workspace/"
}

func (m *MockClient) SelfDestructCmd(providerInstanceID string) string {
	if m.SelfDestructCmdVal != "" {
		return m.SelfDestructCmdVal
	}
	return "echo mock-self-destruct"
}
