//
// Copyright (c) 2018 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	"golang.org/x/sys/unix"

	pb "github.com/kata-containers/agent/protocols/grpc"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
)

// Network fully describes a sandbox network with its interfaces, routes and dns
// related information.
type network struct {
	ifacesLock sync.Mutex
	ifaces     map[string]*pb.Interface

	routesLock sync.Mutex
	routes     []pb.Route

	dns []string
}

////////////////
// Interfaces //
////////////////

func linkByHwAddr(netHandle *netlink.Handle, hwAddr string) (netlink.Link, error) {
	links, err := netHandle.LinkList()
	if err != nil {
		return nil, err
	}

	for _, link := range links {
		lAttrs := link.Attrs()
		if lAttrs == nil {
			continue
		}

		if lAttrs.HardwareAddr.String() == hwAddr {
			return link, nil
		}
	}

	return nil, grpcStatus.Errorf(codes.NotFound, "Could not find the link corresponding to HwAddr %q", hwAddr)
}

func updateLink(netHandle *netlink.Handle, link netlink.Link, iface *pb.Interface) error {

	// As a first step, clear out any existing addresses associated with the link:
	linkIPs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return grpcStatus.Errorf(codes.Internal, "Could not check initial addresses for the link: %v", err)
	}
	for _, linkIP := range linkIPs {
		if err := netlink.AddrDel(link, &linkIP); err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not delete existing addresses: %v", err)
		}
	}

	// Set desired IP addresses:
	for _, addr := range iface.IPAddresses {
		netlinkAddrStr := fmt.Sprintf("%s/%s", addr.Address, addr.Mask)
		netlinkAddr, err := netlink.ParseAddr(netlinkAddrStr)

		if err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not parse %q: %v", netlinkAddrStr, err)
		}

		if err := netHandle.AddrAdd(link, netlinkAddr); err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not add %s to interface %v: %v",
				netlinkAddrStr, link, err)
		}
	}

	// set the interface name:
	if err := netHandle.LinkSetName(link, iface.Name); err != nil {
		return grpcStatus.Errorf(codes.Internal, "Could not set name %s for interface %v: %v", iface.Name, link, err)
	}

	// set the interface MTU:
	if err := netHandle.LinkSetMTU(link, int(iface.Mtu)); err != nil {
		return grpcStatus.Errorf(codes.Internal, "Could not set MTU %d for interface %v: %v", iface.Mtu, link, err)
	}

	return nil
}

func (s *sandbox) addInterface(netHandle *netlink.Handle, iface *pb.Interface) (resultingIfc *pb.Interface, err error) {
	s.network.ifacesLock.Lock()
	defer s.network.ifacesLock.Unlock()

	if netHandle == nil {
		netHandle, err = netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return nil, err
		}
		defer netHandle.Delete()
	}

	if iface == nil {
		return nil, grpcStatus.Errorf(codes.InvalidArgument, "Provided interface is nil")
	}

	hwAddr, err := net.ParseMAC(iface.HwAddr)
	if err != nil {
		return nil, err
	}

	link := &netlink.Device{
		LinkAttrs: netlink.LinkAttrs{
			MTU:          int(iface.Mtu),
			TxQLen:       -1,
			Name:         iface.Name,
			HardwareAddr: hwAddr,
		},
	}

	// Create the link.
	if err := netHandle.LinkAdd(link); err != nil {
		return nil, err
	}

	// Set the link up.
	if err := netHandle.LinkSetUp(link); err != nil {
		return iface, err
	}

	// Update sandbox interface list.
	s.network.ifaces[iface.Name] = iface

	return iface, nil
}
func (s *sandbox) removeInterface(netHandle *netlink.Handle, iface *pb.Interface) (resultingIfc *pb.Interface, err error) {
	s.network.ifacesLock.Lock()
	defer s.network.ifacesLock.Unlock()

	if netHandle == nil {
		netHandle, err = netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return nil, err
		}
		defer netHandle.Delete()
	}

	// Find the interface by hardware address.
	link, err := linkByHwAddr(netHandle, iface.HwAddr)
	if err != nil {
		return nil, grpcStatus.Errorf(codes.Internal, "removeInterface: %v", err)
	}

	// Set the link down.
	if err := netHandle.LinkSetDown(link); err != nil {
		return iface, err
	}

	// Delete the link.
	if err := netHandle.LinkDel(link); err != nil {
		return iface, err
	}

	// Update sandbox interface list.
	delete(s.network.ifaces, iface.Name)

	return nil, nil
}

// updateInterface will update an existing interface with the values provided in the pb.Interface.  It will identify the
// existing interface via MAC address and will return the state of the interface once the function completes as well an any
// errors observed.
func (s *sandbox) updateInterface(netHandle *netlink.Handle, iface *pb.Interface) (resultingIfc *pb.Interface, err error) {
	s.network.ifacesLock.Lock()
	defer s.network.ifacesLock.Unlock()

	if iface == nil {
		return nil, grpcStatus.Errorf(codes.InvalidArgument, "Provided interface is nil")
	}

	if netHandle == nil {
		netHandle, err = netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return nil, err
		}
		defer netHandle.Delete()
	}

	fieldLogger := agentLog.WithFields(logrus.Fields{
		"mac-address":    iface.HwAddr,
		"interface-name": iface.Device,
	})

	var link netlink.Link
	if iface.HwAddr != "" {
		fieldLogger.Info("Getting interface from MAC address")

		// Find the interface link from its hardware address.
		link, err = linkByHwAddr(netHandle, iface.HwAddr)
		if err != nil {
			return nil, grpcStatus.Errorf(codes.Internal, "updateInterface: %v", err)
		}
	} else {
		return nil, grpcStatus.Errorf(codes.InvalidArgument, "Interface HwAddr empty")
	}

	// Use defer function to create and return the interface's state in
	// gRPC agent protocol format in the event that an error is observed
	defer func() {
		if err != nil {
			resultingIfc, _ = getInterface(netHandle, link)
		} else {
			resultingIfc = iface
		}
		//put the link back into the up state
		retErr := netHandle.LinkSetUp(link)

		//if we failed to setup the link but already are returning
		//with an error, return the original error
		if err == nil {
			err = retErr
		}
	}()

	fieldLogger.WithField("link", fmt.Sprintf("%+v", link)).Info("Link found")

	lAttrs := link.Attrs()
	if lAttrs != nil && (lAttrs.Flags&net.FlagUp) == net.FlagUp {
		// The link is up, makes sure we get it down before
		// doing any modification.
		if err = netHandle.LinkSetDown(link); err != nil {
			return
		}
	}

	err = updateLink(netHandle, link, iface)

	return

}

// getInterface will retrieve interface details from the provided link
func getInterface(netHandle *netlink.Handle, link netlink.Link) (*pb.Interface, error) {
	var ifc pb.Interface
	linkAttrs := link.Attrs()
	ifc.Name = linkAttrs.Name
	ifc.Mtu = uint64(linkAttrs.MTU)
	ifc.HwAddr = linkAttrs.HardwareAddr.String()

	addrs, err := netHandle.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		agentLog.WithError(err).Error("getInterface() failed")
		return nil, err
	}
	for _, addr := range addrs {
		netMask, _ := addr.Mask.Size()
		m := pb.IPAddress{
			Address: addr.IP.String(),
			Mask:    fmt.Sprintf("%d", netMask),
		}
		ifc.IPAddresses = append(ifc.IPAddresses, &m)
	}

	return &ifc, nil
}

////////////
// Routes //
////////////

//updateRoutes will take requestedRoutes and create netlink routes, with a goal of creating a final
// state which matches the requested routes.  In doing this, preesxisting non-loopback routes will be
// removed from the network.  If an error occurs, this function returns the list of routes in
// gRPC-route format at the time of failure
func (s *sandbox) updateRoutes(netHandle *netlink.Handle, requestedRoutes *pb.Routes) (resultingRoutes *pb.Routes, err error) {

	if netHandle == nil {
		netHandle, err = netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return nil, err
		}
		defer netHandle.Delete()
	}

	//If we are returning an error, return the current routes on the system
	defer func() {
		if err != nil {
			resultingRoutes, _ = getCurrentRoutes(netHandle)
		}
	}()

	//
	// First things first, let's blow away all the existing routes.  The updateRoutes function
	// is designed to be declarative, so we will attempt to create state matching what is
	// requested, and in the event that we fail to do so, will return the error and final state.
	//

	initRouteList, err := netHandle.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err
	}
	for _, initRoute := range initRouteList {
		// don't delete routes associated with lo:
		link, _ := netHandle.LinkByIndex(initRoute.LinkIndex)
		if link.Attrs().Name == "lo" || link.Attrs().Name == "::1" {
			continue
		}

		err = netHandle.RouteDel(&initRoute)
		if err != nil {
			//If there was an error deleting some of the initial routes,
			//return the error and the current routes on the system via
			//the defer function
			return
		}
	}

	//
	// Set each of the requested routes
	//
	// First make sure we set the interfaces initial routes, as otherwise we
	// won't be able to access the gateway
	for _, reqRoute := range requestedRoutes.Routes {
		if reqRoute.Gateway == "" {
			err = s.updateRoute(netHandle, reqRoute, true)
			if err != nil {
				agentLog.WithError(err).Error("update Route failed")
				//If there was an error setting the route, return the error
				//and the current routes on the system via the defer func
				return
			}

		}
	}
	// Take a second pass and apply the routes which include a gateway
	for _, reqRoute := range requestedRoutes.Routes {
		if reqRoute.Gateway != "" {
			err = s.updateRoute(netHandle, reqRoute, true)
			if err != nil {
				agentLog.WithError(err).Error("update Route failed")
				//If there was an error setting the route, return the
				//error and the current routes on the system via defer
				return
			}
		}
	}

	return requestedRoutes, err
}

//getCurrentRoutes is a helper to gather existing routes in gRPC protocol format
func getCurrentRoutes(netHandle *netlink.Handle) (*pb.Routes, error) {

	if netHandle == nil {
		netHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return nil, err
		}
		defer netHandle.Delete()
	}

	var routes pb.Routes

	finalRouteList, err := netHandle.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return &routes, err
	}

	for _, route := range finalRouteList {
		var r pb.Route
		if route.Dst != nil {
			r.Dest = route.Dst.String()
		}

		if route.Gw != nil {
			r.Gateway = route.Gw.String()
		}

		if route.Src != nil {
			r.Source = route.Src.String()
		}

		r.Scope = uint32(route.Scope)

		link, err := netHandle.LinkByIndex(route.LinkIndex)
		if err != nil {
			return &routes, err
		}
		r.Device = link.Attrs().Name

		routes.Routes = append(routes.Routes, &r)
	}

	return &routes, nil
}

func (s *sandbox) updateRoute(netHandle *netlink.Handle, route *pb.Route, add bool) (err error) {
	s.network.routesLock.Lock()
	defer s.network.routesLock.Unlock()

	if netHandle == nil {
		netHandle, err = netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return err
		}
		defer netHandle.Delete()
	}

	if route == nil {
		return grpcStatus.Error(codes.InvalidArgument, "Provided route is nil")
	}

	// Find link index from route's device name.
	link, err := netHandle.LinkByName(route.Device)
	if err != nil {
		return grpcStatus.Errorf(codes.Internal, "Could not find link from device %s: %v", route.Device, err)
	}

	linkAttrs := link.Attrs()
	if linkAttrs == nil {
		return grpcStatus.Errorf(codes.Internal, "Could not get link's attributes for device %s", route.Device)
	}

	var dst *net.IPNet
	if route.Dest == "default" || route.Dest == "" {
		dst = nil
	} else {
		_, dst, err = net.ParseCIDR(route.Dest)
		if err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not parse route destination %s: %v", route.Dest, err)
		}
	}

	netRoute := &netlink.Route{
		LinkIndex: linkAttrs.Index,
		Dst:       dst,
		Src:       net.ParseIP(route.Source),
		Gw:        net.ParseIP(route.Gateway),
		Scope:     netlink.Scope(route.Scope),
	}

	if add {
		if err := netHandle.RouteAdd(netRoute); err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not add route dest(%s)/gw(%s)/dev(%s): %v",
				route.Dest, route.Gateway, route.Device, err)
		}

		// Add route to sandbox route list.
		s.network.routes = append(s.network.routes, *route)
	} else {
		if err := netHandle.RouteDel(netRoute); err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not remove route dest(%s)/gw(%s)/dev(%s): %v",
				route.Dest, route.Gateway, route.Device, err)
		}

		// Remove route from sandbox route list.
		for idx, sandboxRoute := range s.network.routes {
			if reflect.DeepEqual(sandboxRoute, route) {
				s.network.routes = append(s.network.routes[:idx], s.network.routes[idx+1:]...)
				break
			}
		}
	}

	return nil
}

/////////
// DNS //
/////////

func setupDNS(dns []string) error {
	return nil
}

////////////
// Global //
////////////

// Remove everything related to network.
func (s *sandbox) removeNetwork() error {
	netHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer netHandle.Delete()

	for _, iface := range s.network.ifaces {
		if _, err := s.removeInterface(netHandle, iface); err != nil {
			return grpcStatus.Errorf(codes.Internal, "Could not remove network interface %v: %v",
				iface, err)
		}
	}

	return nil
}
