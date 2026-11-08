// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"context"
	"fmt"

	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var ScheduleGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v1",
	Resource: "schedules",
}

// VeleroClient abstracts CRUD operations on Velero Schedule resources.
type VeleroClient interface {
	ListSchedules(ctx context.Context, namespace, labelSelector string) ([]velerov1.Schedule, error)
	CreateSchedule(ctx context.Context, sch *velerov1.Schedule) (*velerov1.Schedule, error)
	UpdateSchedule(ctx context.Context, sch *velerov1.Schedule) (*velerov1.Schedule, error)
	DeleteSchedule(ctx context.Context, namespace, name string) error
}

// DynamicVeleroClient implements VeleroClient using dynamic.Interface.
type DynamicVeleroClient struct {
	dynClient dynamic.Interface
}

// NewDynamicVeleroClient creates a DynamicVeleroClient.
func NewDynamicVeleroClient(dynClient dynamic.Interface) *DynamicVeleroClient {
	return &DynamicVeleroClient{dynClient: dynClient}
}

// ListSchedules retrieves all Velero schedules matching the given labelSelector in the namespace.
func (c *DynamicVeleroClient) ListSchedules(ctx context.Context, namespace, labelSelector string) ([]velerov1.Schedule, error) {
	unstructuredList, err := c.dynClient.Resource(ScheduleGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing Velero schedules: %w", err)
	}

	var schedules []velerov1.Schedule
	for _, item := range unstructuredList.Items {
		var sch velerov1.Schedule
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &sch); err != nil {
			return nil, fmt.Errorf("decoding schedule %s: %w", item.GetName(), err)
		}
		schedules = append(schedules, sch)
	}
	return schedules, nil
}

// CreateSchedule creates a new Velero Schedule object.
func (c *DynamicVeleroClient) CreateSchedule(ctx context.Context, sch *velerov1.Schedule) (*velerov1.Schedule, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sch)
	if err != nil {
		return nil, fmt.Errorf("converting schedule %s to unstructured: %w", sch.Name, err)
	}

	u := &unstructured.Unstructured{Object: content}
	res, err := c.dynClient.Resource(ScheduleGVR).Namespace(sch.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating schedule %s: %w", sch.Name, err)
	}

	var created velerov1.Schedule
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, &created); err != nil {
		return nil, fmt.Errorf("decoding created schedule: %w", err)
	}
	return &created, nil
}

// UpdateSchedule updates an existing Velero Schedule object.
func (c *DynamicVeleroClient) UpdateSchedule(ctx context.Context, sch *velerov1.Schedule) (*velerov1.Schedule, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sch)
	if err != nil {
		return nil, fmt.Errorf("converting schedule %s to unstructured: %w", sch.Name, err)
	}

	u := &unstructured.Unstructured{Object: content}
	res, err := c.dynClient.Resource(ScheduleGVR).Namespace(sch.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating schedule %s: %w", sch.Name, err)
	}

	var updated velerov1.Schedule
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, &updated); err != nil {
		return nil, fmt.Errorf("decoding updated schedule: %w", err)
	}
	return &updated, nil
}

// DeleteSchedule deletes a Schedule object.
func (c *DynamicVeleroClient) DeleteSchedule(ctx context.Context, namespace, name string) error {
	return c.dynClient.Resource(ScheduleGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}
