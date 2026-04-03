# OTel Tracing

Nerdbox supports OpenTelemetry tracing.

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
