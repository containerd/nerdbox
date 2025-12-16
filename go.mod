module github.com/containerd/nerdbox

go 1.24.3

toolchain go1.24.4

require (
	github.com/containerd/cgroups/v3 v3.1.2
	github.com/containerd/console v1.0.5
	github.com/containerd/containerd/api v1.10.0
	github.com/containerd/containerd/v2 v2.1.4
	github.com/containerd/errdefs v1.0.0
	github.com/containerd/errdefs/pkg v0.3.0
	github.com/containerd/fifo v1.1.0
	github.com/containerd/go-runc v1.1.0
	github.com/containerd/log v0.1.0
	github.com/containerd/otelttrpc v0.1.0
	github.com/containerd/plugin v1.0.0
	github.com/containerd/ttrpc v1.2.7
	github.com/containerd/typeurl/v2 v2.2.3
	github.com/docker/go-events v0.0.0-20190806004212-e31b211e4f1c
	github.com/ebitengine/purego v0.9.1
	github.com/insomniacslk/dhcp v0.0.0-20250919081422-f80a1952f48e
	github.com/mdlayher/vsock v1.2.1
	github.com/moby/sys/userns v0.1.0
	github.com/opencontainers/runtime-spec v1.3.0
	github.com/vishvananda/netlink v1.3.1
	github.com/vishvananda/netns v0.0.5
	golang.org/x/sync v0.19.0
	golang.org/x/sys v0.39.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/Microsoft/hcsshim v0.14.0-rc.1 // indirect
	github.com/cilium/ebpf v0.16.0 // indirect
	github.com/containerd/continuity v0.4.5 // indirect
	github.com/coreos/go-systemd/v22 v22.6.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/josharian/native v1.1.0 // indirect
	github.com/mdlayher/packet v1.1.2 // indirect
	github.com/mdlayher/socket v0.5.1 // indirect
	github.com/moby/sys/mountinfo v0.7.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/u-root/uio v0.0.0-20230220225925-ffce2a382923 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.37.0 // indirect
	go.opentelemetry.io/otel/metric v1.37.0 // indirect
	go.opentelemetry.io/otel/trace v1.37.0 // indirect
	golang.org/x/exp v0.0.0-20241108190413-2d47ceb2692f // indirect
	golang.org/x/net v0.43.0 // indirect
	golang.org/x/text v0.28.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250707201910-8d1bb00bc6a7 // indirect
	google.golang.org/grpc v1.75.1 // indirect
)

replace github.com/containerd/containerd/v2 => github.com/dmcgowan/containerd/v2 v2.2.0-beta.0-erofs-darwin.3
