/*
Copyright 2016 The Kubernetes Authors.

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

package openstack

import (
	"context"
	"errors"
	"net"

	"github.com/asaskevich/govalidator"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/routers"
	neutronports "github.com/gophercloud/gophercloud/openstack/networking/v2/ports"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/cloudprovider"
)

var errNoRouterID = errors.New("router-id not set in cloud provider config")

// Routes implements the cloudprovider.Routes for OpenStack clouds
type Routes struct {
	compute *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
	opts    RouterOpts
}

// NewRoutes creates a new instance of Routes
func NewRoutes(compute *gophercloud.ServiceClient, network *gophercloud.ServiceClient, opts RouterOpts) (cloudprovider.Routes, error) {
	if opts.RouterID == "" {
		return nil, errNoRouterID
	}

	return &Routes{
		compute: compute,
		network: network,
		opts:    opts,
	}, nil
}

// ListRoutes lists all managed routes that belong to the specified clusterName
func (r *Routes) ListRoutes(ctx context.Context, clusterName string) ([]*cloudprovider.Route, error) {
	glog.V(4).Infof("ListRoutes(%v)", clusterName)

	nodeNamesByAddr := make(map[string]types.NodeName)
	err := foreachServer(r.compute, servers.ListOpts{}, func(srv *servers.Server) (bool, error) {
		addrs, err := nodeAddresses(srv)
		if err != nil {
			return false, err
		}

		name := mapServerToNodeName(srv)
		for _, addr := range addrs {
			nodeNamesByAddr[addr.Address] = name
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	router, err := routers.Get(r.network, r.opts.RouterID).Extract()
	if err != nil {
		return nil, err
	}

	var routes []*cloudprovider.Route
	for _, item := range router.Routes {
		nodeName, foundNode := nodeNamesByAddr[item.NextHop]
		route := cloudprovider.Route{
			Name:            item.DestinationCIDR,
			TargetNode:      nodeName, //empty if NextHop is unknown
			Blackhole:       !foundNode,
			DestinationCIDR: item.DestinationCIDR,
		}
		routes = append(routes, &route)
	}

	return routes, nil
}

func updateRoutes(network *gophercloud.ServiceClient, router *routers.Router, newRoutes []routers.Route) (func(), error) {
	origRoutes := router.Routes // shallow copy

	_, err := routers.Update(network, router.ID, routers.UpdateOpts{
		Routes: newRoutes,
	}).Extract()
	if err != nil {
		return nil, err
	}

	unwinder := func() {
		glog.V(4).Info("Reverting routes change to router ", router.ID)
		_, err := routers.Update(network, router.ID, routers.UpdateOpts{
			Routes: origRoutes,
		}).Extract()
		if err != nil {
			glog.Warning("Unable to reset routes during error unwind: ", err)
		}
	}

	return unwinder, nil
}

func updateAllowedAddressPairs(network *gophercloud.ServiceClient, port *neutronports.Port, newPairs []neutronports.AddressPair) (func(), error) {
	origPairs := port.AllowedAddressPairs // shallow copy

	_, err := neutronports.Update(network, port.ID, neutronports.UpdateOpts{
		AllowedAddressPairs: &newPairs,
	}).Extract()
	if err != nil {
		return nil, err
	}

	unwinder := func() {
		glog.V(4).Info("Reverting allowed-address-pairs change to port ", port.ID)
		_, err := neutronports.Update(network, port.ID, neutronports.UpdateOpts{
			AllowedAddressPairs: &origPairs,
		}).Extract()
		if err != nil {
			glog.Warning("Unable to reset allowed-address-pairs during error unwind: ", err)
		}
	}

	return unwinder, nil
}

// CreateRoute creates the described managed route
func (r *Routes) CreateRoute(ctx context.Context, clusterName string, nameHint string, route *cloudprovider.Route) error {
	glog.V(4).Infof("CreateRoute(%v, %v, %v)", clusterName, nameHint, route)

	onFailure := newCaller()

	IP, _, _ := net.ParseCIDR(route.DestinationCIDR)
	CIDRisV4 := govalidator.IsIPv4(IP.String())
	CIDRisV6 := govalidator.IsIPv6(IP.String())
	addrs, err := getAddressesByName(r.compute, route.TargetNode)

	if err != nil {
		return err
	} else if len(addrs) == 0 {
		return ErrNoAddressFound
	}

	var nexthop string = ""

	for _, addr := range addrs {
		if addr.Type == v1.NodeInternalIP {
			if (govalidator.IsIPv4(addr.Address) && CIDRisV4) || (govalidator.IsIPv6(addr.Address) && CIDRisV6) {
				nexthop = addr.Address
				break
			}
		}
	}
	if nexthop == "" {
		return ErrNoAddressFound
	}

	glog.V(4).Infof("Using nexthop %v for node %v", nexthop, route.TargetNode)

	router, err := routers.Get(r.network, r.opts.RouterID).Extract()
	if err != nil {
		return err
	}

	routes := router.Routes

	for _, item := range routes {
		if item.DestinationCIDR == route.DestinationCIDR && item.NextHop == nexthop {
			glog.V(4).Infof("Skipping existing route: %v", route)
			return nil
		}
	}

	routes = append(routes, routers.Route{
		DestinationCIDR: route.DestinationCIDR,
		NextHop:         nexthop,
	})

	unwind, err := updateRoutes(r.network, router, routes)
	if err != nil {
		return err
	}
	defer onFailure.call(unwind)

	// get the port of addr on target node.
	portID, err := getPortIDByIP(r.compute, route.TargetNode, nexthop)
	if err != nil {
		return err
	}
	port, err := getPortByID(r.network, portID)
	if err != nil {
		return err
	}

	found := false
	for _, item := range port.AllowedAddressPairs {
		if item.IPAddress == route.DestinationCIDR {
			glog.V(4).Info("Found existing allowed-address-pair: ", item)
			found = true
			break
		}
	}

	if !found {
		newPairs := append(port.AllowedAddressPairs, neutronports.AddressPair{
			IPAddress: route.DestinationCIDR,
		})
		unwind, err := updateAllowedAddressPairs(r.network, port, newPairs)
		if err != nil {
			return err
		}
		defer onFailure.call(unwind)
	}

	glog.V(4).Infof("Route created: %v", route)
	onFailure.disarm()
	return nil
}

// DeleteRoute deletes the specified managed route
func (r *Routes) DeleteRoute(ctx context.Context, clusterName string, route *cloudprovider.Route) error {
	glog.V(4).Infof("DeleteRoute(%v, %v)", clusterName, route)

	onFailure := newCaller()

	IP, _, _ := net.ParseCIDR(route.DestinationCIDR)
	CIDRisV4 := govalidator.IsIPv4(IP.String())
	CIDRisV6 := govalidator.IsIPv6(IP.String())

	addrs, err := getAddressesByName(r.compute, route.TargetNode)

	if err != nil {
		return err
	} else if len(addrs) == 0 {
		return ErrNoAddressFound
	}

	var nexthop string = ""

	for _, addr := range addrs {
		if addr.Type == v1.NodeInternalIP {
			if (govalidator.IsIPv4(addr.Address) && CIDRisV4) || (govalidator.IsIPv6(addr.Address) && CIDRisV6) {
				nexthop = addr.Address
				break
			}
		}
	}
	if nexthop == "" {
		return ErrNoAddressFound
	}

	router, err := routers.Get(r.network, r.opts.RouterID).Extract()
	if err != nil {
		return err
	}

	routes := router.Routes
	index := -1
	for i, item := range routes {
		if item.DestinationCIDR == route.DestinationCIDR && item.NextHop == nexthop {
			index = i
			break
		}
	}

	if index == -1 {
		glog.V(4).Infof("Skipping non-existent route: %v", route)
		return nil
	}

	// Delete element `index`
	routes[index] = routes[len(routes)-1]
	routes = routes[:len(routes)-1]

	unwind, err := updateRoutes(r.network, router, routes)
	if err != nil {
		return err
	}
	defer onFailure.call(unwind)

	// get the port of nexthop on target node.
	portID, err := getPortIDByIP(r.compute, route.TargetNode, nexthop)
	if err != nil {
		return err
	}
	port, err := getPortByID(r.network, portID)
	if err != nil {
		return err
	}

	addrPairs := port.AllowedAddressPairs
	index = -1
	for i, item := range addrPairs {
		if item.IPAddress == route.DestinationCIDR {
			index = i
			break
		}
	}

	if index != -1 {
		// Delete element `index`
		addrPairs[index] = addrPairs[len(addrPairs)-1]
		addrPairs = addrPairs[:len(addrPairs)-1]

		unwind, err := updateAllowedAddressPairs(r.network, port, addrPairs)
		if err != nil {
			return err
		}
		defer onFailure.call(unwind)
	}

	glog.V(4).Infof("Route deleted: %v", route)
	onFailure.disarm()
	return nil
}

func getPortIDByIP(compute *gophercloud.ServiceClient, targetNode types.NodeName, ipAddress string) (string, error) {
	srv, err := getServerByName(compute, targetNode, true)
	if err != nil {
		return "", err
	}

	interfaces, err := getAttachedInterfacesByID(compute, srv.ID)
	if err != nil {
		return "", err
	}

	for _, intf := range interfaces {
		for _, fixedIP := range intf.FixedIPs {
			if fixedIP.IPAddress == ipAddress {
				return intf.PortID, nil
			}
		}
	}

	return "", ErrNotFound
}

func getPortByID(client *gophercloud.ServiceClient, portID string) (*neutronports.Port, error) {
	targetPort, err := neutronports.Get(client, portID).Extract()
	if err != nil {
		return nil, err
	}

	if targetPort == nil {
		return nil, ErrNotFound
	}

	return targetPort, nil
}
