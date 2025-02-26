/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstacktasks

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/openstack"
)

// +kops:fitask
type LB struct {
	ID            *string
	Name          *string
	Subnet        *string
	VipSubnet     *string
	Lifecycle     fi.Lifecycle
	PortID        *string
	SecurityGroup *SecurityGroup
	Provider      *string
	FlavorID      *string
}

const (
	// loadbalancerActive* is configuration of exponential backoff for
	// going into ACTIVE loadbalancer provisioning status. Starting with 1
	// seconds, multiplying by 1.2 with each step and taking 22 steps at maximum
	// it will time out after 326s, which roughly corresponds to about 5 minutes
	loadbalancerActiveInitDelay = 1 * time.Second
	loadbalancerActiveFactor    = 1.2
	loadbalancerActiveSteps     = 22

	activeStatus = "ACTIVE"
	errorStatus  = "ERROR"
)

func waitLoadbalancerActiveProvisioningStatus(client *gophercloud.ServiceClient, loadbalancerID string) (string, error) {
	backoff := wait.Backoff{
		Duration: loadbalancerActiveInitDelay,
		Factor:   loadbalancerActiveFactor,
		Steps:    loadbalancerActiveSteps,
	}

	var provisioningStatus string
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		loadbalancer, err := loadbalancers.Get(context.TODO(), client, loadbalancerID).Extract()
		if err != nil {
			return false, err
		}
		provisioningStatus = loadbalancer.ProvisioningStatus
		if loadbalancer.ProvisioningStatus == activeStatus {
			return true, nil
		} else if loadbalancer.ProvisioningStatus == errorStatus {
			return true, fmt.Errorf("loadbalancer has gone into ERROR state")
		} else {
			klog.Infof("Waiting for Loadbalancer to be ACTIVE...")
			return false, nil
		}
	})

	if err == wait.ErrWaitTimeout {
		err = fmt.Errorf("loadbalancer failed to go into ACTIVE provisioning status within allotted time")
	}
	return provisioningStatus, err
}

// GetDependencies returns the dependencies of the Instance task
func (e *LB) GetDependencies(tasks map[string]fi.CloudupTask) []fi.CloudupTask {
	var deps []fi.CloudupTask
	for _, task := range tasks {
		if _, ok := task.(*Subnet); ok {
			deps = append(deps, task)
		}
		if _, ok := task.(*SecurityGroup); ok {
			deps = append(deps, task)
		}
	}
	return deps
}

var _ fi.CompareWithID = &LB{}

func (s *LB) CompareWithID() *string {
	return s.ID
}

func NewLBTaskFromCloud(cloud openstack.OpenstackCloud, lifecycle fi.Lifecycle, lb *loadbalancers.LoadBalancer, find *LB) (*LB, error) {
	osCloud := cloud
	sub, err := subnets.Get(context.TODO(), osCloud.NetworkingClient(), lb.VipSubnetID).Extract()
	if err != nil {
		return nil, err
	}

	secGroup := true
	if find != nil && find.SecurityGroup == nil {
		secGroup = false
	}

	actual := &LB{
		ID:        fi.PtrTo(lb.ID),
		Name:      fi.PtrTo(lb.Name),
		Lifecycle: lifecycle,
		PortID:    fi.PtrTo(lb.VipPortID),
		Subnet:    fi.PtrTo(sub.Name),
		VipSubnet: fi.PtrTo(lb.VipSubnetID),
		Provider:  fi.PtrTo(lb.Provider),
		FlavorID:  fi.PtrTo(lb.FlavorID),
	}

	if secGroup {
		sg, err := getSecurityGroupByName(&SecurityGroup{Name: fi.PtrTo(lb.Name)}, osCloud)
		if err != nil {
			return nil, err
		}
		actual.SecurityGroup = sg
	}
	if find != nil {
		find.ID = actual.ID
		find.PortID = actual.PortID
		find.VipSubnet = actual.VipSubnet
		find.Provider = actual.Provider
		find.FlavorID = actual.FlavorID
	}
	return actual, nil
}

func (s *LB) Find(context *fi.CloudupContext) (*LB, error) {
	if s.Name == nil {
		return nil, nil
	}

	cloud := context.T.Cloud.(openstack.OpenstackCloud)
	lbPage, err := loadbalancers.List(cloud.LoadBalancerClient(), loadbalancers.ListOpts{
		Name: fi.ValueOf(s.Name),
	}).AllPages(context.Context())
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve loadbalancers for name %s: %v", fi.ValueOf(s.Name), err)
	}
	lbs, err := loadbalancers.ExtractLoadBalancers(lbPage)
	if err != nil {
		return nil, fmt.Errorf("Failed to extract loadbalancers : %v", err)
	}
	if len(lbs) == 0 {
		return nil, nil
	}
	if len(lbs) > 1 {
		return nil, fmt.Errorf("Multiple load balancers for name %s", fi.ValueOf(s.Name))
	}

	return NewLBTaskFromCloud(cloud, s.Lifecycle, &lbs[0], s)
}

func (s *LB) Run(context *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(s, context)
}

func (_ *LB) CheckChanges(a, e, changes *LB) error {
	if a == nil {
		if e.Name == nil {
			return fi.RequiredField("Name")
		}
	} else {
		if changes.ID != nil {
			return fi.CannotChangeField("ID")
		}
		if changes.Name != nil {
			return fi.CannotChangeField("Name")
		}
	}
	return nil
}

func (_ *LB) RenderOpenstack(t *openstack.OpenstackAPITarget, a, e, changes *LB) error {
	if a == nil {
		klog.V(2).Infof("Creating LB with Name: %q", fi.ValueOf(e.Name))

		subnets, err := t.Cloud.ListSubnets(subnets.ListOpts{
			Name: fi.ValueOf(e.Subnet),
		})
		if err != nil {
			return fmt.Errorf("Failed to retrieve subnet `%s` in loadbalancer creation: %v", fi.ValueOf(e.Subnet), err)
		}
		if len(subnets) != 1 {
			return fmt.Errorf("Unexpected desired subnets for `%s`.  Expected 1, got %d", fi.ValueOf(e.Subnet), len(subnets))
		}

		lbopts := loadbalancers.CreateOpts{
			Name:        fi.ValueOf(e.Name),
			VipSubnetID: subnets[0].ID,
		}
		if e.FlavorID != nil {
			lbopts.FlavorID = fi.ValueOf(e.FlavorID)
		}
		lb, err := t.Cloud.CreateLB(lbopts)
		if err != nil {
			return fmt.Errorf("error creating LB: %v", err)
		}
		e.ID = fi.PtrTo(lb.ID)
		e.PortID = fi.PtrTo(lb.VipPortID)
		e.VipSubnet = fi.PtrTo(lb.VipSubnetID)
		e.Provider = fi.PtrTo(lb.Provider)
		e.FlavorID = fi.PtrTo(lb.FlavorID)

		if e.SecurityGroup != nil {
			opts := ports.UpdateOpts{
				SecurityGroups: &[]string{fi.ValueOf(e.SecurityGroup.ID)},
			}
			_, err = ports.Update(context.TODO(), t.Cloud.NetworkingClient(), lb.VipPortID, opts).Extract()
			if err != nil {
				return fmt.Errorf("Failed to update security group for port %s: %v", lb.VipPortID, err)
			}
		}
		return nil
	}
	// We may have failed to update the security groups on the load balancer
	port, err := t.Cloud.GetPort(fi.ValueOf(a.PortID))
	if err != nil {
		return fmt.Errorf("Failed to get port with id %s: %v", fi.ValueOf(a.PortID), err)
	}
	// Ensure the loadbalancer port has one security group and it is the one specified,
	if e.SecurityGroup != nil &&
		(len(port.SecurityGroups) < 1 || port.SecurityGroups[0] != fi.ValueOf(e.SecurityGroup.ID)) {

		opts := ports.UpdateOpts{
			SecurityGroups: &[]string{fi.ValueOf(e.SecurityGroup.ID)},
		}
		_, err = ports.Update(context.TODO(), t.Cloud.NetworkingClient(), fi.ValueOf(a.PortID), opts).Extract()
		if err != nil {
			return fmt.Errorf("Failed to update security group for port %s: %v", fi.ValueOf(a.PortID), err)
		}
		return nil
	}

	klog.V(2).Infof("Openstack task LB::RenderOpenstack did nothing")
	return nil
}
