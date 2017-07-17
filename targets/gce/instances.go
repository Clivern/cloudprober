// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gce

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/google/cloudprober/logger"
	dnsRes "github.com/google/cloudprober/targets/resolver"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v0.alpha"
)

// globalInstancesProvider is a singleton instance of the instancesProvider struct.
// It is presented as a singleton because, like forwardingRules, instances provides
// a cache layer that is best shared by all probes.
var (
	// Mutex to safely initialize the globalInstanceProvider
	globalInstancesProviderMu sync.Mutex
	globalInstancesProvider   *instancesProvider
)

// instances represents GCE instances. To avoid making GCE API calls for each
// set of GCE instances targets, for example for VM-to-VM probes over internal IP
// and public IP, we use a global instances provider (globalInstancesProvider).
type instances struct {
	pb *Instances
	r  *dnsRes.Resolver
}

// newInstances returns a new instances object. It will initialize
// globalInstancesProvider if needed.
func newInstances(project string, reEvalInterval time.Duration, ipb *Instances, globalResolver *dnsRes.Resolver, l *logger.Logger) (*instances, error) {
	if ipb.GetNetworkInterface() != nil && ipb.GetUseDnsToResolve() {
		return nil, errors.New("network_intf and use_dns_to_resolve are mutually exclusive")
	}
	if ipb.GetUseDnsToResolve() && globalResolver == nil {
		return nil, errors.New("use_dns_to_resolve configured, but globalResolver is nil")
	}
	// Initialize global instances provider if not already initialized.
	if err := initGlobalInstancesProvider(project, reEvalInterval, l); err != nil {
		return nil, err
	}
	return &instances{
		pb: ipb,
		r:  globalResolver,
	}, nil
}

// List produces a list of all instances. This list is similar to running
// "gcloud compute instances list", but with a cache layer reducing the number
// of actual API calls made.
func (i *instances) List() []string {
	return globalInstancesProvider.list()
}

// Resolve resolves the name into an IP address. Unless explicitly configured
// to use DNS, we use the instance object (retrieved through GCE API) to
// determine the instance IPs.
func (i *instances) Resolve(name string, ipVer int) (net.IP, error) {
	if i.pb.GetUseDnsToResolve() {
		return i.r.Resolve(name, ipVer)
	}
	ins := globalInstancesProvider.get(name)
	if ins == nil {
		return nil, fmt.Errorf("gce.instances.resolve(%s): instance not in in-memory GCE instances database", name)
	}
	niIndex := 0
	ipType := Instances_NetworkInterface_PRIVATE
	ni := i.pb.GetNetworkInterface()
	if ni != nil {
		niIndex = int(ni.GetIndex())
		ipType = ni.GetIpType()
	}
	if len(ins.NetworkInterfaces) <= niIndex {
		return nil, fmt.Errorf("gce.instances.resolve(%s): no network interface at index: %d", name, niIndex)
	}
	intf := ins.NetworkInterfaces[niIndex]
	switch ipType {
	case Instances_NetworkInterface_PRIVATE:
		return net.ParseIP(intf.NetworkIP), nil
	case Instances_NetworkInterface_PUBLIC:
		if len(intf.AccessConfigs) == 0 {
			return nil, fmt.Errorf("gce.instances.resolve(%s): no access config, instance most likely doesn't have a public IP", name)
		}
		return net.ParseIP(intf.AccessConfigs[0].NatIP), nil
	case Instances_NetworkInterface_ALIAS:
		if len(intf.AliasIpRanges) == 0 {
			return nil, fmt.Errorf("gce.instances.resolve(%s): no alias IP range", name)
		}
		// Compute API allows specifying CIDR range as an IP address, try that first.
		if ip := net.ParseIP(intf.AliasIpRanges[0].IpCidrRange); ip != nil {
			return ip, nil
		}
		ip, _, err := net.ParseCIDR(intf.AliasIpRanges[0].IpCidrRange)
		return ip, err
	}
	return nil, fmt.Errorf("gce.instances.resolve(%s): unknown IP type for network interface", name)
}

// instancesProvider is a lister which lists GCE instances. There is supposed to
// be only one instancesProvider object per cloudprober instance:
// globalInstancesProvider. It implements a cache, that's populated at a regular
// interval (configured by GlobalGCETargetsOptions.re_eval_sec
// cloudprober/targets/targets.proto) by making GCE API calls. Listing actually
// only returns the current contents of that cache.
type instancesProvider struct {
	project      string
	thisInstance string
	l            *logger.Logger

	mu    sync.RWMutex // Mutex for names and cache
	names []string
	cache map[string]*compute.Instance
}

func initGlobalInstancesProvider(project string, reEvalInterval time.Duration, l *logger.Logger) error {
	globalInstancesProviderMu.Lock()
	defer globalInstancesProviderMu.Unlock()

	if globalInstancesProvider != nil {
		return nil
	}

	var thisInstance string
	if metadata.OnGCE() {
		var err error
		thisInstance, err = metadata.InstanceName()
		if err != nil {
			return fmt.Errorf("initGlobalInstancesProvider: error while getting current instance name: %v", err)
		}
		l.Infof("initGlobalInstancesProvider: this instance: %s", thisInstance)
	}
	globalInstancesProvider = &instancesProvider{
		project:      project,
		thisInstance: thisInstance,
		cache:        make(map[string]*compute.Instance),
		l:            l,
	}
	go func() {
		globalInstancesProvider.expand()
		for _ = range time.Tick(reEvalInterval) {
			globalInstancesProvider.expand()
		}
	}()
	return nil
}

// get returns compute.Instance resource from the cache by name.
func (ip *instancesProvider) get(name string) *compute.Instance {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	return ip.cache[name]
}

func (ip *instancesProvider) list() []string {
	ip.mu.RLock()
	defer ip.mu.RUnlock()
	return append([]string{}, ip.names...)
}

// listInstances runs equivalent API calls as "gcloud compute instances list",
// and is what is used to populate the cache.
func listInstances(project string) ([]*compute.Instance, error) {
	client, err := google.DefaultClient(oauth2.NoContext, compute.ComputeScope)
	if err != nil {
		return nil, err
	}
	cs, err := compute.New(client)
	if err != nil {
		return nil, err
	}
	zonesList, err := cs.Zones.List(project).Do()
	if err != nil {
		return nil, err
	}
	var result []*compute.Instance
	var instanceList *compute.InstanceList
	for _, zone := range zonesList.Items {
		instanceList, err = cs.Instances.List(project, zone.Name).Do()
		if err != nil {
			return nil, err
		}
		result = append(result, instanceList.Items...)
	}
	return result, nil
}

// expand will refill the cache, and update names.
func (ip *instancesProvider) expand() {
	ip.l.Infof("gce.instances.expand: expanding GCE targets")

	computeInstances, err := listInstances(ip.project)
	if err != nil {
		ip.l.Errorf("gce.instances.expand: error while getting list of all instances: %v", err)
		return
	}

	var result []string
	ip.mu.Lock()
	defer ip.mu.Unlock()
	for _, ins := range computeInstances {
		if ins.Name == ip.thisInstance {
			continue
		}
		ip.cache[ins.Name] = ins
		result = append(result, ins.Name)
	}

	ip.l.Debugf("Expanded target list: %q", result)
	ip.names = result
}