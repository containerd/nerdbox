# Bind Mounts

Nerdbox supports bind mounts from the host into containers running inside the
VM. Bind mounts are implemented using virtiofs to share host paths with the VM,
which then bind-mounts them into containers.

## How It Works

When a bind mount is specified in the container spec:

1. The shim transforms the bind mount into a virtiofs share
2. The host path is shared with the VM via virtiofs with a unique tag
3. Inside the VM, virtiofs is mounted at a temporary location (`/mnt/bind-{hash}`)
4. The container runtime bind-mounts from that location into the container

## Directory Bind Mounts

For directory bind mounts, the directory is shared directly via virtiofs:

```
Host: /host/data/ → virtiofs share → VM: /mnt/bind-{hash}/ → Container: /container/data/
```

## File Bind Mounts

When bind-mounting a single file, nerdbox shares the **parent directory** of
the file via virtiofs, then bind-mounts the specific file into the container:

```
Host: /host/config/app.yaml
  ↓
virtiofs shares: /host/config/  (parent directory)
  ↓
VM: /mnt/bind-{hash}/app.yaml
  ↓
Container: /container/app.yaml
```

### Security Implications

When using file bind mounts, be aware that the **entire parent directory** is
exposed to the VM, not just the single file. This has security implications if
the VM is considered a security boundary:

- All files in the parent directory become accessible to the VM
- If an attacker compromises the VM, they can access any file in that directory
- Sensitive files that happen to be siblings of the bind-mounted file are exposed

**Recommendations:**

- Avoid bind-mounting files from directories containing secrets, credentials,
  or sensitive data
- If the VM is treated as a security boundary, audit what gets exposed when
  using file bind mounts
- Place files intended for bind-mounting in dedicated directories with no other
  sensitive content
- Consider using directory bind mounts with only the necessary files instead of
  file bind mounts
