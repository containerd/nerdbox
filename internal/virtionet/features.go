package virtionet

import "fmt"

// This list of features is taken from https://github.com/containers/libkrun/blob/357ec63fee444b973e4fc76d2121fd41631f121e/include/libkrun.h#L274
const (
	VIRTIO_NET_F_CSUM       uint32 = 1 << 0  // Device handles packets with partial checksum offload.
	VIRTIO_NET_F_GUEST_CSUM uint32 = 1 << 1  // Driver handles packets with partial checksum.
	VIRTIO_NET_F_GUEST_TSO4 uint32 = 1 << 7  // Driver can receive TSOv4.
	VIRTIO_NET_F_GUEST_TSO6 uint32 = 1 << 8  // Driver can receive TSOv6.
	VIRTIO_NET_F_GUEST_UFO  uint32 = 1 << 10 // Driver can receive UFO.
	VIRTIO_NET_F_HOST_TSO4  uint32 = 1 << 11 // Device can receive TSOv4.
	VIRTIO_NET_F_HOST_TSO6  uint32 = 1 << 12 // Device can receive TSOv6.
	VIRTIO_NET_F_HOST_UFO   uint32 = 1 << 14 // Device can receive UFO.
)

type Features uint32

func FeaturesFromStrings(features ...string) (Features, error) {
	return Features(0).Add(features...)
}

func (f Features) Add(features ...string) (Features, error) {
	for _, feature := range features {
		switch feature {
		case "VIRTIO_NET_F_CSUM":
			f = Features(uint32(f) | VIRTIO_NET_F_CSUM)
		case "VIRTIO_NET_F_GUEST_CSUM":
			f = Features(uint32(f) | VIRTIO_NET_F_GUEST_CSUM)
		case "VIRTIO_NET_F_GUEST_TSO4":
			f = Features(uint32(f) | VIRTIO_NET_F_GUEST_TSO4)
		case "VIRTIO_NET_F_GUEST_TSO6":
			f = Features(uint32(f) | VIRTIO_NET_F_GUEST_TSO6)
		case "VIRTIO_NET_F_GUEST_UFO":
			f = Features(uint32(f) | VIRTIO_NET_F_GUEST_UFO)
		case "VIRTIO_NET_F_HOST_TSO4":
			f = Features(uint32(f) | VIRTIO_NET_F_HOST_TSO4)
		case "VIRTIO_NET_F_HOST_TSO6":
			f = Features(uint32(f) | VIRTIO_NET_F_HOST_TSO6)
		case "VIRTIO_NET_F_HOST_UFO":
			f = Features(uint32(f) | VIRTIO_NET_F_HOST_UFO)
		default:
			return 0, fmt.Errorf("unknown feature: %s", feature)
		}
	}

	return f, nil
}

func (f Features) AsUint32() uint32 {
	return uint32(f)
}

func (f Features) Features() []string {
	var features []string
	if uint32(f)&VIRTIO_NET_F_CSUM > 0 {
		features = append(features, "VIRTIO_NET_F_CSUM")
	}
	if uint32(f)&VIRTIO_NET_F_GUEST_CSUM > 0 {
		features = append(features, "VIRTIO_NET_F_GUEST_CSUM")
	}
	if uint32(f)&VIRTIO_NET_F_GUEST_TSO4 > 0 {
		features = append(features, "VIRTIO_NET_F_GUEST_TSO4")
	}
	if uint32(f)&VIRTIO_NET_F_GUEST_TSO6 > 0 {
		features = append(features, "VIRTIO_NET_F_GUEST_TSO6")
	}
	if uint32(f)&VIRTIO_NET_F_GUEST_UFO > 0 {
		features = append(features, "VIRTIO_NET_F_GUEST_UFO")
	}
	if uint32(f)&VIRTIO_NET_F_HOST_TSO4 > 0 {
		features = append(features, "VIRTIO_NET_F_HOST_TSO4")
	}
	if uint32(f)&VIRTIO_NET_F_HOST_TSO6 > 0 {
		features = append(features, "VIRTIO_NET_F_HOST_TSO6")
	}
	if uint32(f)&VIRTIO_NET_F_HOST_UFO > 0 {
		features = append(features, "VIRTIO_NET_F_HOST_UFO")
	}
	return features
}
