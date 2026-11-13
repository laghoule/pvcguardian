// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"context"
	"fmt"
	"sync"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
)

// FakeVeleroClient implements VeleroClient in-memory for testing.
type FakeVeleroClient struct {
	mu        sync.RWMutex
	Schedules map[string]*velerov1.Schedule

	// Optional injected failures, used to exercise reconciliation error paths.
	ListErr   error
	CreateErr error
	UpdateErr error
	DeleteErr error
}

// NewFakeVeleroClient returns an initialized in-memory Velero client.
func NewFakeVeleroClient() *FakeVeleroClient {
	return &FakeVeleroClient{
		Schedules: make(map[string]*velerov1.Schedule),
	}
}

func (f *FakeVeleroClient) ListSchedules(ctx context.Context, namespace, labelSelector string) ([]velerov1.Schedule, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.ListErr != nil {
		return nil, f.ListErr
	}

	var list []velerov1.Schedule
	for _, sch := range f.Schedules {
		if sch.Namespace == namespace {
			// Basic label check for managed-by
			if labelSelector != "" {
				// simplistic selector support for testing
				if sch.Labels != nil && sch.Labels[HelmManagedByLabel] == HelmManagedByValue {
					list = append(list, *sch.DeepCopy())
				} else if sch.Labels != nil && sch.Labels["app.kubernetes.io/managed-by"] == "pvcguardian" {
					list = append(list, *sch.DeepCopy())
				}
			} else {
				list = append(list, *sch.DeepCopy())
			}
		}
	}
	return list, nil
}

func (f *FakeVeleroClient) CreateSchedule(ctx context.Context, sch *velerov1.Schedule) (*velerov1.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.CreateErr != nil {
		return nil, f.CreateErr
	}

	key := fmt.Sprintf("%s/%s", sch.Namespace, sch.Name)
	if _, exists := f.Schedules[key]; exists {
		return nil, fmt.Errorf("schedule %s already exists", key)
	}

	copied := sch.DeepCopy()
	f.Schedules[key] = copied
	return copied, nil
}

func (f *FakeVeleroClient) UpdateSchedule(ctx context.Context, sch *velerov1.Schedule) (*velerov1.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.UpdateErr != nil {
		return nil, f.UpdateErr
	}

	key := fmt.Sprintf("%s/%s", sch.Namespace, sch.Name)
	if _, exists := f.Schedules[key]; !exists {
		return nil, fmt.Errorf("schedule %s not found", key)
	}

	copied := sch.DeepCopy()
	f.Schedules[key] = copied
	return copied, nil
}

func (f *FakeVeleroClient) DeleteSchedule(ctx context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.DeleteErr != nil {
		return f.DeleteErr
	}

	key := fmt.Sprintf("%s/%s", namespace, name)
	if _, exists := f.Schedules[key]; !exists {
		return fmt.Errorf("schedule %s not found", key)
	}
	delete(f.Schedules, key)
	return nil
}
