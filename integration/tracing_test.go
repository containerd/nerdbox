//go:build tracing

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

package integration

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	bundleapi "github.com/containerd/nerdbox/api/services/bundle/v1"
	systemapi "github.com/containerd/nerdbox/api/services/system/v1"
	tracesapi "github.com/containerd/nerdbox/api/services/traces/v1"
	"github.com/containerd/nerdbox/internal/nwcfg"
	"github.com/containerd/nerdbox/internal/tracing"
	"github.com/containerd/nerdbox/internal/vm"
)

// TestTraceVMBoot boots a VM and exercises the TTRPC API with tracing
// exported to a local Jaeger instance (localhost:4318).
//
// Run with: make test-tracing
func TestTraceVMBoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// Point tracing at the local Jaeger instance.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	// Initialise the internal tracing package (sets up OTLP export).
	shutdownTracing := tracing.Init(ctx, "nerdbox")
	defer func() {
		// Give the background flusher time to batch and send spans.
		time.Sleep(1 * time.Second)
		if err := shutdownTracing(ctx); err != nil {
			t.Logf("tracing shutdown: %v", err)
		}
	}()

	for _, backend := range vmBackends {
		t.Run(backend.name, func(t *testing.T) {
			traceVMBoot(ctx, t, backend.vmm)
		})
	}

	t.Log("Traces sent to Jaeger. Open http://localhost:16686 to view.")
}

func traceVMBoot(ctx context.Context, t *testing.T, vmm vm.Manager) {
	td := t.TempDir()
	t.Chdir(td)
	// Use Getwd to resolve symlinks (e.g., /var -> /private/var on macOS)
	resolvedTd, err := os.Getwd()
	if err != nil {
		t.Fatal("failed to resolve temp dir:", err)
	}

	// Span: VM.NewInstance+Start — this becomes the trace root.
	ctx, vmSpan := tracing.Start(ctx, "VM.NewInstance+Start")
	instance, err := vmm.NewInstance(ctx, resolvedTd)
	if err != nil {
		vmSpan.End()
		t.Fatal("failed to create VM instance:", err)
	}
	if err := instance.Start(ctx); err != nil {
		vmSpan.End()
		t.Fatal("failed to start VM:", err)
	}
	hostBootTime := time.Now() // Sync point: ttrpc is responsive.
	vmSpan.End()

	defer instance.Shutdown(t.Context())

	client := instance.Client()

	// Subscribe to VM trace stream and relay spans to our exporter.
	trc, err := tracesapi.NewTTRPCTracesClient(client).Stream(ctx, &ptypes.Empty{})
	if err != nil {
		t.Logf("warning: failed to subscribe to VM trace stream: %v", err)
	} else {
		go tracing.ForwardTraces(ctx, trc, tracing.ParseOTLPEndpoint(), hostBootTime)
	}

	// Span: TTRPC.System.Info
	infoCtx, infoSpan := tracing.Start(ctx, "TTRPC.System.Info")
	ss := systemapi.NewTTRPCSystemClient(client)
	resp, err := ss.Info(infoCtx, nil)
	infoSpan.End()
	if err != nil {
		t.Fatal("failed to get system info:", err)
	}
	t.Logf("System info: version=%s kernel=%s", resp.Version, resp.KernelVersion)

	// Build a minimal OCI bundle with config.json and network config.
	// The container uses /bin/sh from the VM's initrd (no separate rootfs).
	// Minimal OCI spec that crun can process. The bundle service creates
	// rootfs/ as an empty dir; we pass a bind mount of / to populate it.
	ociSpec := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"args": []string{"/sbin/crun", "--version"},
			"cwd":  "/",
		},
		"root": map[string]any{
			"path":     "rootfs",
			"readonly": false,
		},
		"mounts": []map[string]any{
			{"destination": "/proc", "type": "proc", "source": "proc"},
			{"destination": "/dev", "type": "tmpfs", "source": "tmpfs"},
			{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"ro"}},
		},
		"linux": map[string]any{
			"namespaces": []map[string]string{
				{"type": "pid"},
				{"type": "network"},
				{"type": "mount"},
			},
		},
	}
	configJSON, err := json.Marshal(ociSpec)
	if err != nil {
		t.Fatal("marshaling OCI spec:", err)
	}

	// Empty network config — Connect will return nil (no networks).
	nwCfg := nwcfg.Config{}
	nwJSON, err := json.Marshal(nwCfg)
	if err != nil {
		t.Fatal("marshaling nw config:", err)
	}

	// Span: TTRPC.Bundle.Create
	bundleCtx, bundleSpan := tracing.Start(ctx, "TTRPC.Bundle.Create")
	bs := bundleapi.NewTTRPCBundleClient(client)
	br, err := bs.Create(bundleCtx, &bundleapi.CreateRequest{
		ID: "trace-test-ctr",
		Files: map[string][]byte{
			"config.json":  configJSON,
			nwcfg.Filename: nwJSON,
		},
	})
	bundleSpan.End()
	if err != nil {
		t.Fatal("failed to create bundle:", err)
	}
	t.Logf("Bundle created at %s", br.Bundle)

	// Span: TTRPC.Task.Create — exercises runc.NewContainer, crun.create,
	// ctrnetworking.Connect, and all the VM-side spans we added.
	createCtx, createSpan := tracing.Start(ctx, "TTRPC.Task.Create")
	tc := taskAPI.NewTTRPCTaskClient(client)
	createResp, err := tc.Create(createCtx, &taskAPI.CreateTaskRequest{
		ID:     "trace-test-ctr",
		Bundle: br.Bundle,
		Rootfs: []*types.Mount{
			{
				Type:    "bind",
				Source:  "/",
				Options: []string{"rbind", "ro"},
			},
		},
	})
	createSpan.End()
	if err != nil {
		t.Fatalf("Task.Create failed: %v", err)
	}
	t.Logf("Task created with PID %d", createResp.Pid)

	// Span: TTRPC.Task.Start — exercises container.Start, crun.start.
	startCtx, startSpan := tracing.Start(ctx, "TTRPC.Task.Start")
	startResp, err := tc.Start(startCtx, &taskAPI.StartRequest{
		ID: "trace-test-ctr",
	})
	startSpan.End()
	if err != nil {
		t.Fatalf("Task.Start failed: %v", err)
	}
	t.Logf("Task started with PID %d", startResp.Pid)

	// Wait for VM-side batcher to flush spans through the relay.
	time.Sleep(1 * time.Second)
}
