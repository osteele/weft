package vastai

import "time"

// MockClient is a test double for VastaiClient.
// Each method delegates to its corresponding Func field; if the field is nil,
// the method returns zero values.
type MockClient struct {
	AvailableFunc            func() error
	SearchOffersFunc         func(OfferConstraints) ([]Offer, error)
	CreateInstanceFunc       func(int, CreateOpts) (*Instance, error)
	ShowInstanceFunc         func(int) (*Instance, error)
	ListAllInstancesFunc     func() ([]Instance, error)
	WaitReadyFunc            func(int, time.Duration) (*Instance, error)
	DestroyInstanceFunc      func(int) error
	CopyBetweenInstancesFunc func(int, string, int, string) error
}

var _ VastaiClient = (*MockClient)(nil)

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

func (m *MockClient) CreateInstance(offerID int, opts CreateOpts) (*Instance, error) {
	if m.CreateInstanceFunc != nil {
		return m.CreateInstanceFunc(offerID, opts)
	}
	return nil, nil
}

func (m *MockClient) ShowInstance(instanceID int) (*Instance, error) {
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

func (m *MockClient) WaitReady(instanceID int, timeout time.Duration) (*Instance, error) {
	if m.WaitReadyFunc != nil {
		return m.WaitReadyFunc(instanceID, timeout)
	}
	return nil, nil
}

func (m *MockClient) DestroyInstance(instanceID int) error {
	if m.DestroyInstanceFunc != nil {
		return m.DestroyInstanceFunc(instanceID)
	}
	return nil
}

func (m *MockClient) CopyBetweenInstances(srcID int, srcPath string, dstID int, dstPath string) error {
	if m.CopyBetweenInstancesFunc != nil {
		return m.CopyBetweenInstancesFunc(srcID, srcPath, dstID, dstPath)
	}
	return nil
}
