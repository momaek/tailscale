// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package portmapper

import (
	"context"
	"time"

	"github.com/tailscale/goupnp/dcps/internetgateway2"
	"golang.org/x/sync/errgroup"
	"inet.af/netaddr"
)

// References:
//
// WANIP Connection v2: http://upnp.org/specs/gw/UPnP-gw-WANIPConnection-v2-Service.pdf

// upnpMapping is a port mapping over the upnp protocol. After being created it is immutable,
// but the client field may be shared across mapping instances.
type upnpMapping struct {
	gw         netaddr.IP
	external   netaddr.IPPort
	internal   netaddr.IPPort
	goodUntil  time.Time
	renewAfter time.Time

	// client is a connection to a upnp device, and may be reused across different UPnP mappings.
	client upnpClient
}

func (u *upnpMapping) GoodUntil() time.Time     { return u.goodUntil }
func (u *upnpMapping) RenewAfter() time.Time    { return u.renewAfter }
func (u *upnpMapping) External() netaddr.IPPort { return u.external }
func (u *upnpMapping) release(ctx context.Context) {
	u.client.DeletePortMapping(ctx, "", u.external.Port(), "udp")
}

// upnpClient is an interface over the multiple different clients exported by goupnp,
// exposing the functions we need for portmapping. They are auto-generated from XML-specs.
type upnpClient interface {
	AddPortMapping(
		ctx context.Context,

		// remoteHost is the remote device sending packets to this device, in the format of x.x.x.x.
		// The empty string, "", means any host out on the internet can send packets in.
		remoteHost string,

		// externalPort is the exposed port of this port mapping. Visible during NAT operations.
		// 0 will let the router select the port, but there is an additional call,
		// `AddAnyPortMapping`, which is available on 1 of the 3 possible protocols,
		// which should be used if available. See `addAnyPortMapping` below, which calls this if
		// `AddAnyPortMapping` is not supported.
		externalPort uint16,

		// protocol is whether this is over TCP or UDP. Either "tcp" or "udp".
		protocol string,

		// internalPort is the port that the gateway device forwards the traffic to.
		internalPort uint16,
		// internalClient is the IP address that packets will be forwarded to for this mapping.
		// Internal client is of the form "x.x.x.x".
		internalClient string,

		// enabled is whether this portmapping should be enabled or disabled.
		enabled bool,
		// portMappingDescription is a user-readable description of this portmapping.
		portMappingDescription string,
		// leaseDurationSec is the duration of this portmapping. The value of this argument must be
		// greater than 0. From the spec, it appears if it is set to 0, it will switch to using
		// 604800 seconds, but not sure why this is desired. The recommended time is 3600 seconds.
		leaseDurationSec uint32,
	) (err error)

	DeletePortMapping(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddress(ctx context.Context) (externalIPAddress string, err error)
}

const tsPortMappingDesc = "tailscale-portmap"

// addAnyPortMapping abstracts over different UPnP client connections, calling the available
// AddAnyPortMapping call if available for WAN IP connection v2, otherwise defaulting to the old
// behavior of calling AddPortMapping with port = 0 to specify a wildcard port.
func addAnyPortMapping(
	ctx context.Context,
	upnp upnpClient,
	externalPort uint16,
	internalPort uint16,
	internalClient string,
	leaseDuration time.Duration,
) (newPort uint16, err error) {
	if upnp, ok := upnp.(*internetgateway2.WANIPConnection2); ok {
		return upnp.AddAnyPortMapping(
			ctx,
			"",
			externalPort,
			"udp",
			internalPort,
			internalClient,
			true,
			tsPortMappingDesc,
			uint32(leaseDuration.Seconds()),
		)
	}
	err = upnp.AddPortMapping(
		ctx,
		"",
		externalPort,
		"udp",
		internalPort,
		internalClient,
		true,
		tsPortMappingDesc,
		uint32(leaseDuration.Seconds()),
	)
	return internalPort, err
}

// getUPnPClients gets a client for interfacing with UPnP, ignoring the underlying protocol for
// now.
// Adapted from https://github.com/huin/goupnp/blob/master/GUIDE.md.
func getUPnPClient(ctx context.Context) (upnpClient, error) {
	tasks, _ := errgroup.WithContext(ctx)
	// Attempt to connect over the multiple available connection types concurrently,
	// returning the fastest.
	var ip1Clients []*internetgateway2.WANIPConnection1
	var errs [3]error
	tasks.Go(func() error {
		var err error
		ip1Clients, _, err = internetgateway2.NewWANIPConnection1Clients(ctx)
		errs[0] = err
		return nil
	})
	var ip2Clients []*internetgateway2.WANIPConnection2
	tasks.Go(func() error {
		var err error
		ip2Clients, _, err = internetgateway2.NewWANIPConnection2Clients(ctx)
		errs[1] = err
		return nil
	})
	var ppp1Clients []*internetgateway2.WANPPPConnection1
	tasks.Go(func() error {
		var err error
		ppp1Clients, _, err = internetgateway2.NewWANPPPConnection1Clients(ctx)
		errs[2] = err
		return nil
	})

	err := tasks.Wait()

	switch {
	case len(ip2Clients) > 0:
		return ip2Clients[0], nil
	case len(ip1Clients) > 0:
		return ip1Clients[0], nil
	case len(ppp1Clients) > 0:
		return ppp1Clients[0], nil
	default:
		if err != nil {
			return nil, err
		}
		for i := range errs {
			if errs[i] != nil {
				err = errs[i]
				break
			}
		}
		// Didn't get any outputs, report if there was an error or nil if
		// just no clients.
		return nil, err
	}
}

// getUPnPPortMapping attempts to create a port-mapping over the UPnP protocol. On success,
// it will return the externally exposed IP and port. Otherwise, it will return a zeroed IP and
// port and an error.
func (c *Client) getUPnPPortMapping(
	ctx context.Context,
	gw netaddr.IP,
	internal netaddr.IPPort,
	prevPort uint16,
) (external netaddr.IPPort, err error) {
	now := time.Now()
	upnp := &upnpMapping{
		gw:       gw,
		internal: internal,
	}

	var client upnpClient
	c.mu.Lock()
	oldMapping, ok := c.mapping.(*upnpMapping)
	c.mu.Unlock()
	if ok && oldMapping != nil {
		client = oldMapping.client
	} else {
		client, err = getUPnPClient(ctx)
		if err != nil {
			return netaddr.IPPort{}, NoMappingError{ErrNoPortMappingServices}
		}
	}
	if client == nil {
		return netaddr.IPPort{}, NoMappingError{ErrNoPortMappingServices}
	}

	var newPort uint16
	newPort, err = addAnyPortMapping(
		ctx,
		client,
		prevPort,
		internal.Port(),
		internal.IP().String(),
		time.Second*pmpMapLifetimeSec,
	)
	if err != nil {
		return netaddr.IPPort{}, NoMappingError{ErrNoPortMappingServices}
	}
	// TODO cache this ip somewhere?
	extIP, err := client.GetExternalIPAddress(ctx)
	if err != nil {
		// TODO this doesn't seem right
		return netaddr.IPPort{}, NoMappingError{ErrNoPortMappingServices}
	}
	externalIP, err := netaddr.ParseIP(extIP)
	if err != nil {
		return netaddr.IPPort{}, NoMappingError{ErrNoPortMappingServices}
	}

	upnp.external = netaddr.IPPortFrom(externalIP, newPort)
	d := time.Duration(pmpMapLifetimeSec) * time.Second
	upnp.goodUntil = now.Add(d)
	upnp.renewAfter = now.Add(d / 2)
	upnp.client = client
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mapping = upnp
	c.localPort = newPort
	return upnp.external, nil
}
