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
	"net"
	"net/netip"
	"time"

	"github.com/containerd/log"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func configureDHCP(ctx context.Context, iface netlink.Link, nw Network, debug bool) (_ *DHCPLease, retErr error) {
	if err := netlink.LinkSetUp(iface); err != nil {
		log.G(ctx).WithError(err).Error("failed to bring up virtio interface")
		return nil, err
	}

	log.G(ctx).Debug("brought up virtio interface")

	copts := []nclient4.ClientOpt{
		nclient4.WithLogger(dhcpLogger{}),
		nclient4.WithSummaryLogger(),
	}
	if debug {
		copts = append(copts, nclient4.WithDebugLogger())
	}

	c, err := nclient4.New(iface.Attrs().Name, copts...)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to create DHCP client")
		return nil, err
	}
	defer c.Close()

	l, err := c.Request(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to request DHCP lease")
		return nil, err
	}
	log.G(ctx).WithFields(log.Fields{
		"lease": l.ACK.String(),
	}).Debug("received DHCP lease")

	// Past this point, release the lease if there's an error.
	defer func() {
		if retErr != nil {
			if err := c.Release(l); err != nil {
				log.G(ctx).WithFields(log.Fields{
					"err":      err,
					"orig_err": retErr,
					"lease":    l.ACK.String(),
				}).Error("failed to release DHCP lease")
			}
		}
	}()

	if err := netlink.AddrAdd(iface, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   l.ACK.YourIPAddr,
			Mask: l.ACK.SubnetMask(),
		},
		// Disable DAD to avoid random delays until the IP address is ready
		// and the VM gets external connectivity.
		// The VMM, and its network provider, need to ensure that there's no
		// conflicting IP addresses assigned to multiple VMs on the same
		// network.
		Flags: unix.IFA_F_PERMANENT | unix.IFA_F_NODAD,
	}); err != nil {
		log.G(ctx).WithError(err).Error("failed to add IP address to virtio interface")
		return nil, err
	}

	return &DHCPLease{client: c, lease: l}, nil
}

// DHCPLease provides a limited implementation of DHCP lease renewal and release.
//
// It provides a limited implementation of the DHCP state machine defined in
// RFC 2131. More specifically, it only implements the RENEWING state defined
// in Section 4.4.5 (see [1]). The REBINDING state is not implemented.
//
// [1]: https://datatracker.ietf.org/doc/html/rfc2131#section-4.4.5.
type DHCPLease struct {
	client *nclient4.Client
	lease  *nclient4.Lease
}

// Routers returns the 'router' option received by the DHCP server.
func (l *DHCPLease) Routers() []netip.Addr {
	if l.lease.ACK.Router() == nil {
		return nil
	}

	var addrs []netip.Addr
	for _, ip := range l.lease.ACK.Router() {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addrs = append(addrs, addr)
	}

	return addrs
}

const defaultRenewalInterval = 86400 * time.Second // 24 hours

// RenewLoop runs the loop that periodically renews the DHCP lease.
func (l *DHCPLease) RenewLoop(ctx context.Context) error {
	for {
		interval := l.lease.ACK.IPAddressRenewalTime(defaultRenewalInterval)
		log.G(ctx).Debugf("renewing DHCP lease in %s", interval)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
			log.G(ctx).Debug("renewing DHCP lease")

			lease, err := l.client.Renew(ctx, l.lease)
			if err != nil {
				return err
			}

			log.G(ctx).WithFields(log.Fields{
				"lease": lease.ACK.String(),
			}).Debug("renewed DHCP lease")

			l.lease = lease
		}
	}
}

// Release releases the DHCP lease.
//
// TODO(aker): Release is never called.
func (l *DHCPLease) Release(ctx context.Context) error {
	if err := l.client.Release(l.lease); err != nil {
		log.G(ctx).WithError(err).Error("failed to release DHCP lease")
		return err
	}

	log.G(ctx).Debug("released DHCP lease")
	return nil
}

// dhcpLogger is a small adapter to use containerd's log package with dhcp
type dhcpLogger struct{}

func (dhcpLogger) PrintMessage(prefix string, message *dhcpv4.DHCPv4) {
	log.G(context.Background()).WithFields(log.Fields{
		"prefix":  prefix,
		"message": message,
	}).Debug("dhcp message")
}

func (dhcpLogger) Printf(format string, v ...interface{}) {
	log.G(context.Background()).Debugf(format, v...)
}
