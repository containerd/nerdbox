# OTel Tracing

Nerdbox supports OpenTelemetry tracing across the full container startup path,
including spans that originate inside the VM.

## Tracing with Jaeger

This requires docker to run the Jaeger service.

```bash
make test-tracing

# Open the Jaeger UI in the browser
make jaeger-open

# Make a Mermaid gantt diagram
make jaeger-gantt

# Stop Jaeger
make jaeger-stop
```

An example gantt looks like this:
```mermaid
gantt
    dateFormat x
    axisFormat %s.%L s
    title Trace for nerdbox-vm

    section VM.NewInstance+Start
    nerdbox/VM.NewInstance+Start :VM_NewInstance+Start_9f6fda, 0, 83
    . nerdbox/libkrun.VMStart :libkrun_VMStart_90885a, 5, 82
    . nerdbox/libkrun.WaitForTTRPC :libkrun_WaitForTTRPC_10af7c, 5, 82
    . nerdbox/TTRPC.Task.Create :TTRPC_Task_Create_5e832c, 83, 85
    . . nerdbox/containerd.task.v3.Task/Create :containerd_task_v3_Task_Create_3a4175, 84, 85
    . . . containerd.task.v3.Task/Create :containerd_task_v3_Task_Create_61f5b3, 83, 85
    . . . . task.Create :task_Create_a0b789, 84, 85
    . . . . . runc.NewContainer :runc_NewContainer_f4a190, 84, 85
    . . . . . . crun.create :crun_create_beaf4f, 84, 85
    . nerdbox/TTRPC.Task.Start :TTRPC_Task_Start_c8de57, 85, 87
    . . nerdbox/containerd.task.v3.Task/Start :containerd_task_v3_Task_Start_f0139b, 85, 87
    . . . containerd.task.v3.Task/Start :containerd_task_v3_Task_Start_1d5297, 85, 87
    . . . . task.Start :task_Start_84961b, 85, 87
    . . . . . . container.Start :container_Start_5372d6, 85, 87
    . . . . . . . crun.start :crun_start_ce160c, 85, 87
```
