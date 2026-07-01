package cloud

import "time"

// MockClient is a test double for cloud.Client.
type MockClient struct {
	ProviderVal                    Provider
	AvailableFunc                  func() error
	SearchOffersFunc               func(OfferConstraints) ([]Offer, error)
	CreateInstanceFunc             func(string, CreateOpts) (*Instance, error)
	CreateInstanceWithProgressFunc func(string, CreateOpts, ProgressFunc) (*Instance, error)
	ShowInstanceFunc               func(string) (*Instance, error)
	ListAllInstancesFunc           func() ([]Instance, error)
	WaitReadyFunc                  func(string, time.Duration) (*Instance, error)
	DestroyInstanceFunc            func(string) error
	ChangeBidFunc                  func(string, float64) error
	CopyBetweenInstancesFunc       func(string, string, string, string) error
	SelfDestructCmdVal             string
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

func (m *MockClient) CreateInstanceWithProgress(offerID string, opts CreateOpts, progress ProgressFunc) (*Instance, error) {
	if m.CreateInstanceWithProgressFunc != nil {
		return m.CreateInstanceWithProgressFunc(offerID, opts, progress)
	}
	return m.CreateInstance(offerID, opts)
}

func (m *MockClient) ShowInstance(instanceID string) (*Instance, error) {
	if m.ShowInstanceFunc != nil {
		return m.ShowInstanceFunc(instanceID)
	}
	return nil, nil
}

func (m *MockClient) ListAllInstances() ([]Instance, error) {
	if m.ListAllInstancesFunc != nil {
		return m.ListAllInstancesFunc()
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

func (m *MockClient) ChangeBid(instanceID string, pricePerHour float64) error {
	if m.ChangeBidFunc != nil {
		return m.ChangeBidFunc(instanceID, pricePerHour)
	}
	return nil
}

func (m *MockClient) CopyBetweenInstances(srcID, srcPath, dstID, dstPath string) error {
	if m.CopyBetweenInstancesFunc != nil {
		return m.CopyBetweenInstancesFunc(srcID, srcPath, dstID, dstPath)
	}
	return nil
}

func (m *MockClient) SelfDestructCmd(providerInstanceID string) string {
	if m.SelfDestructCmdVal != "" {
		return m.SelfDestructCmdVal
	}
	return "echo mock-self-destruct"
}
