/*
 * Start a VM using libkrun
 */

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>
#include <libgen.h>
#include <libkrun.h>
#include <getopt.h>
#include <stdbool.h>
#include <assert.h>
#include <pthread.h>

#define MAX_ARGS_LEN 4096
#ifndef MAX_PATH
#define MAX_PATH 4096
#endif

#if defined(__x86_64__)
#define KERNEL_FORMAT KRUN_KERNEL_FORMAT_ELF
#else
#define KERNEL_FORMAT KRUN_KERNEL_FORMAT_RAW
#endif

static void print_help(char *const name)
{
    fprintf(stderr,
            "Usage: %s [OPTIONS] KERNEL INITRAMFS\n"
            "OPTIONS: \n"
            "        -l    --listen-path         Socket file to connect to\n"
            "        -c    --console-path        Path to the console file for output\n"
            "        -k    --kernel-cmdline      Kernel command line\n"
            "        -d    --data-disk           Path to a data disk in raw format\n"
            "        -v    --virtio-fs-path      Path to a directory to send via virtiofs (tag=path)\n"
            "        -h    --help                Show help\n"
            "        -n    --nested              Enabled nested virtualization\n"
            "\n"
#if defined(__x86_64__)
            "%s-kernel:    path to the kernel image in ELF format\n"
            "%s-initrd:    path to the initramfs image in CPIO format\n",
#else
            "%s-kernel:    path to the kernel image in RAW format\n"
            "%s-initrd:    path to the initramfs image in CPIO format\n",
#endif
            name, name, name);
}

static const struct option long_options[] = {
    {"listen-path", required_argument, NULL, 'l'},
    {"kernel-cmdline", required_argument, NULL, 'k'},
    {"data-disk", required_argument, NULL, 'd'},
    {"virtio-fs", required_argument, NULL, 'v'},
    {"console-path", required_argument, NULL, 'c'},
    {"initrd-path", required_argument, NULL, 'i'},
    {"nested", no_argument, NULL, 'n'},
    {"help", no_argument, NULL, 'h'},
    {NULL, 0, NULL, 0}};

struct cmdline
{
    bool show_help;
    char const *data_disk;
    char const *virtio_fs_tag;
    char const *virtio_fs_path;
    char const *console_path;
    char const *passt_socket_path;
    char const *kernel_path;
    char const *kernel_cmdline;
    char const *initrd_path;
    char const *listen_path;
    char const *stream_path;
    bool nested;
};

bool parse_cmdline(int argc, char *const argv[], struct cmdline *cmdline)
{
    assert(cmdline != NULL);

    // set the defaults
    *cmdline = (struct cmdline){
        .show_help = false,
        .data_disk = NULL,
        .kernel_cmdline = NULL,
        .listen_path = NULL,
        .stream_path = NULL,
        .nested = false,
    };

    int option_index = 0;
    int c;
    // the '+' in optstring is a GNU extension that disables permutating argv
    while ((c = getopt_long(argc, argv, "+hl:s:k:c:d:v:n", long_options, &option_index)) != -1)
    {
        switch (c)
        {
        case 'l':
            cmdline->listen_path = optarg;
            break;
        case 's':
            cmdline->stream_path = optarg;
            break;
        case 'k':
            cmdline->kernel_cmdline = optarg;
            break;
        case 'd':
            cmdline->data_disk = optarg;
            break;
        case 'c':
            cmdline->console_path = optarg;
            break;
        case 'v':
            for (char *p = optarg; *p != '\0'; p++)
            {
                if (*p == '=')
                {
                    *p = '\0';
                    cmdline->virtio_fs_path = p + 1;
                    break;
                }
            }
            cmdline->virtio_fs_tag = optarg;
            break;
        case 'h':
            cmdline->show_help = true;
            return true;
        case 'n':
            cmdline->nested = true;
            break;
        case '?':
            return false;
        default:
            fprintf(stderr, "internal argument parsing error (returned character code 0x%x)\n", c);
            return false;
        }
    }

    return true;
}

int main(int argc, char *const argv[])
{
    int ctx_id;
    int err;
    pthread_t thread;
    char kernel_path[255];
    char initrd_path[255];
    char * bin_path;
    struct cmdline cmdline;

    if (!parse_cmdline(argc, argv, &cmdline))
    {
        putchar('\n');
        print_help(argv[0]);
        return -1;
    }

    if (cmdline.show_help)
    {
        print_help(argv[0]);
        return 0;
    }

    bin_path = dirname(argv[0]);
    // Size must allow "nerdbox-kernel" at end
    if (sizeof(bin_path) > 240) {
        printf("executable path is too long");
        return -1;
    }
    strcat(strcpy(kernel_path, bin_path), "/nerdbox-kernel");
    strcat(strcpy(initrd_path, bin_path), "/nerdbox-initrd");

    fprintf(stderr, "initrd: %s\n", initrd_path);
    fprintf(stderr, "kernel_path: %s\n", kernel_path);
    fprintf(stderr, "kernel_cmdline: %s\n", cmdline.kernel_cmdline);

    // Set the log level to "off".
    err = krun_set_log_level(0);
    if (err)
    {
        errno = -err;
        perror("Error configuring log level");
        return -1;
    }

    // Create the configuration context.
    ctx_id = krun_create_ctx();
    if (ctx_id < 0)
    {
        errno = -ctx_id;
        perror("Error creating configuration context");
        return -1;
    }

    // Configure the number of vCPUs (2) and the amount of RAM (1024 MiB).
    if (err = krun_set_vm_config(ctx_id, 2, 2048))
    {
        errno = -err;
        perror("Error configuring the number of vCPUs and/or the amount of RAM");
        return -1;
    }

    fprintf(stderr, "config created\n");

    fprintf(stderr, "boot disk created\n");

    if (cmdline.data_disk)
    {
        if (err = krun_add_disk(ctx_id, "data", cmdline.data_disk, 0))
        {
            errno = -err,
            perror("Error configuring data disk");
            return -1;
        }
    }

    fprintf(stderr, "setting kernel\n");

    if (err = krun_set_kernel(ctx_id, kernel_path, KERNEL_FORMAT,
                              initrd_path, cmdline.kernel_cmdline))
    {
        errno = -err;
        perror("Error configuring kernel");
        return -1;
    }

    fprintf(stderr, "kernel set\n");

    const char *const init_args[] =
    {
        "-debug",
        "-vsock-rpc-port=1024", // vsock port number
        "-vsock-stream-port=1025", // vsock port number
        "-vsock-cid=3", // vsock guest context id
        0
    };

    const char *const init_env[] =
    {
        "TERM=xterm",
        "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "LANG=C.UTF-8",
        0
    };
    if (err = krun_set_exec(ctx_id, (const char*)"/sbin/vminitd", &init_args[0], &init_env[0]))
    {
        errno = -err;
        perror("Error configuring kernel");
        return -1;
    }   

    fprintf(stderr, "exec set\n");

    if (cmdline.listen_path)
    {
        fprintf(stderr, "listen_path: %s\n", cmdline.listen_path);
        if (err = krun_add_vsock_port2(ctx_id, 1024, cmdline.listen_path, true))
        {
            errno = -err;
            perror("Error configuring vsock port");
            return -1;
        }
    }

    if (cmdline.stream_path)
    {
        fprintf(stderr, "stream_path: %s\n", cmdline.stream_path);
        if (err = krun_add_vsock_port2(ctx_id, 1025, cmdline.stream_path, true))
        {
            errno = -err;
            perror("Error configuring vsock port");
            return -1;
        }
    }

    if (cmdline.console_path)
    {
        fprintf(stderr, "console_path: %s\n", cmdline.console_path);
        if (err = krun_set_console_output(ctx_id, cmdline.console_path))
        {
            errno = -err;
            perror("Error configuring console");
            return -1;
        }
    }

    if (cmdline.virtio_fs_path && cmdline.virtio_fs_tag)
    {
        fprintf(stderr, "virtio_fs_tag: %s\n", cmdline.virtio_fs_tag);
        fprintf(stderr, "virtio_fs_path: %s\n", cmdline.virtio_fs_path);
        if (err = krun_add_virtiofs(ctx_id, cmdline.virtio_fs_tag, cmdline.virtio_fs_path))
        {
            errno = -err;
            perror("Error configuring virtiofs");
            return -1;
        }
    }

    fprintf(stderr, "nested=%d\n", cmdline.nested);
    if (err = krun_set_nested_virt(ctx_id, cmdline.nested))
    {
        errno = -err;
        perror("Error configuring nested virtualization");
        return -1;
    }

    fflush(stderr);

    // Start and enter the microVM. Unless there is some error while creating the microVM
    // this function never returns.
    if (err = krun_start_enter(ctx_id))
    {
        errno = -err;
        perror("Error creating the microVM");
        return -1;
    }

    // Not reached.
    return 0;
}
