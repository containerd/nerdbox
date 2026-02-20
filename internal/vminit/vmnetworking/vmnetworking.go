//go:build linux

/*
   Copyright The containerd Authors.

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

package vmnetworking

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/vishvananda/netlink"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/containerd/nerdbox/internal/sliceutil"
)

type Network struct {
	MAC   net.HardwareAddr
	Addr4 netip.Prefix
	Addr6 netip.Prefix
	DHCP  bool
}

func (nw Network) Validate() error {
	if nw.MAC == nil {
		return errors.New("must specify MAC address")
	}
	if (nw.Addr4.IsValid() || nw.Addr6.IsValid()) && nw.DHCP {
		return errors.New("cannot specify both addr and dhcp")
	}
	return nil
}

func SetupVM(ctx context.Context, nws []Network, debug bool) (func(context.Context) error, func() error, error) {
	ifaces, err := listVirtioIfaces()
	if err != nil {
		return nil, nil, err
	}

	log.G(ctx).WithFields(log.Fields{
		"networks":      nws,
		"virtio_ifaces": ifaces,
	}).Debug("setting up networking")

	// A same MAC address can be specified multiple times if there are multiple
	// IP addresses to assign to the same interface. So, count unique MAC
	// addresses and check that we've enough virtio interfaces.
	nwsCopy := append([]Network{}, nws...)
	uniqueMACs := len(slices.CompactFunc(nwsCopy, func(a, b Network) bool { return a.MAC.String() == b.MAC.String() }))
	if len(ifaces) < uniqueMACs {
		return nil, nil, fmt.Errorf("not enough virtio interfaces found (found %d, expected %d)", len(ifaces), uniqueMACs)
	}

	link, err := netlink.LinkByName("lo")
	if err != nil {
		return nil, nil, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		log.G(ctx).WithFields(log.Fields{
			"err":   err,
			"iface": link.Attrs().Name,
		}).Error("failed to bring up lo interface")
		return nil, nil, err
	}
	log.G(ctx).Debug("brought up lo interface")

	eg, ctx := errgroup.WithContext(ctx)
	var leases []*DHCPLease
	// Initialize gws with a size to ensure that values are appended in the
	// same order as nws.
	gws := make([]netip.Addr, len(nws))
	var mu sync.Mutex // protects leases and gws

	for i, nw := range nws {
		iface, ok := ifaces[nw.MAC.String()]
		if !ok {
			log.G(ctx).WithField("mac", nw.MAC.String()).Error("virtio interface not found")
			continue
		}

		ctx := log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
			"mac":   nw.MAC.String(),
			"iface": iface.Attrs().Name,
		}))

		if nw.DHCP {
			eg.Go(func() error {
				lease, err := configureDHCP(ctx, iface, nw, debug)
				if err != nil {
					return err
				}

				mu.Lock()
				leases = append(leases, lease)
				// If the DHCP lease contains a 'router' option, select the first
				// value as a potential default gateway.
				routers := lease.Routers()
				if len(routers) > 0 {
					gws[i] = routers[0]
				}
				mu.Unlock()

				return nil
			})
		} else {
			eg.Go(func() error {
				ctx := log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
					"addr4": nw.Addr4.String(),
					"addr6": nw.Addr6.String(),
				}))

				// Consider that the 1st assignable IP address in the subnet is
				// the gateway and select that as a potential default gateway.
				if nw.Addr4.IsValid() {
					gws[i] = nw.Addr4.Masked().Addr().Next()
				} else {
					gws[i] = nw.Addr6.Masked().Addr().Next()
				}

				return configureStatic(ctx, iface, nw)
			})
		}
	}

	if err := eg.Wait(); err != nil {
		return nil, nil, err
	}

	// Find the first non-zero gateway address and use it to set up the default
	// route.
	gws = sliceutil.Filter(gws, func(gw netip.Addr) bool { return gw.IsValid() })
	if len(gws) > 0 {
		if err := netlink.RouteAdd(&netlink.Route{
			Scope: unix.RT_SCOPE_UNIVERSE,
			Gw:    gws[0].AsSlice(),
		}); err != nil {
			return nil, nil, fmt.Errorf("failed to add default gateway route: %w", err)
		}
	}

	nameservers := sliceutil.Map(gws, func(gw netip.Addr) string { return fmt.Sprintf("nameserver %s", gw.String()) })
	nameservers = append(nameservers, "") // Trailing newline.
	if err := os.WriteFile("/etc/resolv.conf", []byte(strings.Join(nameservers, "\n")), 0644); err != nil {
		return nil, nil, fmt.Errorf("failed to write resolv.conf: %w", err)
	}
	if err := os.WriteFile("/etc/hosts", []byte("127.0.0.1\tlocalhost\n"), 0644); err != nil {
		return nil, nil, fmt.Errorf("failed to write hosts: %w", err)
	}

	renewer := func(ctx context.Context) error {
		eg, ctx := errgroup.WithContext(ctx)
		for _, lease := range leases {
			eg.Go(func() error { return lease.RenewLoop(ctx) })
		}
		return eg.Wait()
	}
	releaser := func() error {
		// Create a new context without cancellation to make sure DHCP leases
		// are correctly released even if the parent context has been canceled.
		eg, ctx := errgroup.WithContext(context.WithoutCancel(ctx))
		for _, lease := range leases {
			eg.Go(func() error { return lease.Release(ctx) })
		}
		return eg.Wait()
	}

	return renewer, releaser, nil
}

func listVirtioIfaces() (map[string]netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}

	ifaces := map[string]netlink.Link{}
	for _, link := range links {
		if link.Attrs().ParentDevBus == "virtio" {
			ifaces[link.Attrs().HardwareAddr.String()] = link
		}
	}

	return ifaces, nil
}

// configureStatic configures an interface with a static IP address.
func configureStatic(ctx context.Context, iface netlink.Link, nw Network) error {
	for _, prefix := range []netip.Prefix{nw.Addr4, nw.Addr6} {
		if !prefix.IsValid() {
			continue
		}

		if err := netlink.AddrAdd(iface, &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   prefix.Addr().AsSlice(),
				Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
			},
			// Disable DAD to avoid random delays until the IP address is ready
			// and the VM gets external connectivity.
			// The VMM, and its network provider, need to ensure that there's no
			// conflicting IP addresses assigned to multiple VMs on the same
			// network.
			Flags: unix.IFA_F_PERMANENT | unix.IFA_F_NODAD,
		}); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error": err,
				"addr":  prefix.String(),
				"iface": iface.Attrs().Name,
			}).Error("failed to add IP address to virtio interface")
			return err
		}
	}

	if err := netlink.LinkSetUp(iface); err != nil {
		log.G(ctx).WithError(err).Error("failed to bring up virtio interface")
		return err
	}

	log.G(ctx).Debug("brought up virtio interface")

	return nil
}
