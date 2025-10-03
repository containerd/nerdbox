package vmnetworking

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/containerd/log"
	"github.com/vishvananda/netlink"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

type Network struct {
	MAC  net.HardwareAddr
	Addr netip.Prefix
}

func (nw Network) Validate() error {
	if nw.MAC == nil || !nw.Addr.IsValid() {
		return errors.New("must specify mac and addr")
	}
	return nil
}

func SetupVM(ctx context.Context, nws []Network, debug bool) error {
	ifaces, err := listVirtioIfaces()
	if err != nil {
		return err
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
		return fmt.Errorf("not enough virtio interfaces found (found %d, expected %d)", len(ifaces), uniqueMACs)
	}

	link, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		log.G(ctx).WithFields(log.Fields{
			"err":   err,
			"iface": link.Attrs().Name,
		}).Error("failed to bring up lo interface")
		return err
	}
	log.G(ctx).Debug("brought up lo interface")

	eg, ctx := errgroup.WithContext(ctx)

	for _, nw := range nws {
		iface, ok := ifaces[nw.MAC.String()]
		if !ok {
			log.G(ctx).WithField("mac", nw.MAC.String()).Error("virtio interface not found")
			continue
		}

		ctx := log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
			"mac":   nw.MAC.String(),
			"iface": iface.Attrs().Name,
		}))

		eg.Go(func() error {
			ctx := log.WithLogger(ctx, log.G(ctx).WithField("addr", nw.Addr.String()))
			return configureStatic(ctx, iface, nw)
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	if len(nws) > 0 {
		if err := netlink.RouteAdd(&netlink.Route{
			Scope: unix.RT_SCOPE_UNIVERSE,
			// Consider that the 1st assignable IP address in the subnet is
			// the gateway.
			Gw: nws[0].Addr.Masked().Addr().Next().AsSlice(),
		}); err != nil {
			return fmt.Errorf("failed to add default gateway route: %w", err)
		}
	}

	// TODO(aker): write resolv.conf

	return nil
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
	if err := netlink.AddrAdd(iface, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   nw.Addr.Addr().AsSlice(),
			Mask: net.CIDRMask(nw.Addr.Bits(), nw.Addr.Addr().BitLen()),
		},
		// Disable DAD to avoid random delays until the IP address is ready
		// and the VM gets external connectivity.
		// The VMM, and its network provider, need to ensure that there's no
		// conflicting IP addresses assigned to multiple VMs on the same
		// network.
		Flags: unix.IFA_F_PERMANENT | unix.IFA_F_NODAD,
	}); err != nil {
		log.G(ctx).WithError(err).Error("failed to add IP address to virtio interface")
		return err
	}

	if err := netlink.LinkSetUp(iface); err != nil {
		log.G(ctx).WithError(err).Error("failed to bring up virtio interface")
		return err
	}

	log.G(ctx).Debug("brought up virtio interface")

	return nil
}
