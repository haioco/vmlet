package lifecycle

import (
	"context"

	"github.com/haioco/vmlet/internal/api"
	"github.com/haioco/vmlet/internal/models"
)

// Mock implementations for testing and examples

type mockMicroVMManager struct {
	microvms map[string]*models.MicroVMInstance
	status   map[string]models.MicroVMStatus
}

func newMockMicroVMManager() *mockMicroVMManager {
	return &mockMicroVMManager{
		microvms: make(map[string]*models.MicroVMInstance),
		status:   make(map[string]models.MicroVMStatus),
	}
}

func (m *mockMicroVMManager) CreateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	m.microvms[microvm.ID] = microvm
	m.status[microvm.ID] = models.MicroVMStatusRunning
	microvm.Status = models.MicroVMStatusRunning
	return nil
}

func (m *mockMicroVMManager) StartMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	m.status[microvm.ID] = models.MicroVMStatusRunning
	microvm.Status = models.MicroVMStatusRunning
	return nil
}

func (m *mockMicroVMManager) StopMicroVM(ctx context.Context, microvm *models.MicroVMInstance, force bool) error {
	m.status[microvm.ID] = models.MicroVMStatusStopped
	microvm.Status = models.MicroVMStatusStopped
	return nil
}

func (m *mockMicroVMManager) DeleteMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	delete(m.microvms, microvm.ID)
	delete(m.status, microvm.ID)
	return nil
}

func (m *mockMicroVMManager) GetMicroVMStatus(ctx context.Context, microvmID string) (models.MicroVMStatus, error) {
	if status, exists := m.status[microvmID]; exists {
		return status, nil
	}
	return models.MicroVMStatusError, nil
}

type mockStorage struct {
	microvms map[string]*models.MicroVMInstance
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		microvms: make(map[string]*models.MicroVMInstance),
	}
}

func (s *mockStorage) StoreMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	s.microvms[microvm.ID] = microvm
	return nil
}

func (s *mockStorage) GetMicroVM(ctx context.Context, microvmID string) (*models.MicroVMInstance, error) {
	if microvm, exists := s.microvms[microvmID]; exists {
		return microvm, nil
	}
	return nil, models.ErrMicroVMNotFound
}

func (s *mockStorage) UpdateMicroVM(ctx context.Context, microvm *models.MicroVMInstance) error {
	s.microvms[microvm.ID] = microvm
	return nil
}

func (s *mockStorage) DeleteMicroVM(ctx context.Context, microvmID string) error {
	delete(s.microvms, microvmID)
	return nil
}

func (s *mockStorage) ListMicroVMs(ctx context.Context, filters api.StorageFilters) ([]*models.MicroVMInstance, error) {
	var result []*models.MicroVMInstance
	for _, microvm := range s.microvms {
		if filters.Status != "" && microvm.Status != filters.Status {
			continue
		}
		if filters.CustomerID != "" && microvm.Metadata.CustomerID != filters.CustomerID {
			continue
		}
		if filters.NodeID != "" && microvm.NodeID != filters.NodeID {
			continue
		}
		result = append(result, microvm)
	}
	return result, nil
}

func (s *mockStorage) StoreResourcePool(ctx context.Context, pool *models.ResourcePool) error {
	return nil
}

func (s *mockStorage) GetResourcePool(ctx context.Context, nodeID string) (*models.ResourcePool, error) {
	return nil, nil
}

func (s *mockStorage) ListResourcePools(ctx context.Context) ([]*models.ResourcePool, error) {
	return nil, nil
}