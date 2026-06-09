/* nerdbox-nsmount — single-threaded helper for setns(CLONE_NEWNS) + bind mount.
 *
 * vminitd (Go) cannot call setns(2) with CLONE_NEWNS itself: the kernel
 * rejects the syscall from any process whose thread group has more than one
 * member, and the Go runtime spawns sysmon before user code runs. This helper
 * is a tiny static C binary the Go service execs to do the work.
 *
 * Wire format (argv):
 *
 *   nerdbox-nsmount mount  PID SOURCE TARGET [ro]
 *   nerdbox-nsmount umount PID TARGET
 *
 * Exit 0 on success; non-zero with a one-line message on stderr otherwise.
 * The mount target's parent is created with mkdir -p semantics; the target
 * itself is touched if missing (regular file when source is a regular file,
 * directory otherwise). The umount path tolerates EINVAL/ENOENT for
 * idempotence.
 */

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

static int die(const char *msg) {
    fprintf(stderr, "nsmount: %s: %s\n", msg, strerror(errno));
    return 1;
}

static int mkdir_p(const char *path) {
    char buf[4096];
    size_t n = strlen(path);
    if (n >= sizeof(buf)) {
        errno = ENAMETOOLONG;
        return -1;
    }
    memcpy(buf, path, n + 1);
    for (char *p = buf + 1; *p; p++) {
        if (*p == '/') {
            *p = '\0';
            if (mkdir(buf, 0755) < 0 && errno != EEXIST) return -1;
            *p = '/';
        }
    }
    if (mkdir(buf, 0755) < 0 && errno != EEXIST) return -1;
    return 0;
}

static int enter_ns(const char *pid_str) {
    char path[64];
    snprintf(path, sizeof(path), "/proc/%s/ns/mnt", pid_str);
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        fprintf(stderr, "nsmount: open %s: %s\n", path, strerror(errno));
        return -1;
    }
    if (setns(fd, CLONE_NEWNS) < 0) {
        fprintf(stderr, "nsmount: setns %s: %s\n", path, strerror(errno));
        close(fd);
        return -1;
    }
    close(fd);
    return 0;
}

static int do_mount(int argc, char *argv[]) {
    if (argc < 5) {
        fprintf(stderr, "nsmount: mount requires SOURCE TARGET\n");
        return 1;
    }
    const char *source = argv[3];
    const char *target = argv[4];
    int readonly = (argc >= 6 && strcmp(argv[5], "ro") == 0);

    /* Parent. */
    char parent[4096];
    size_t n = strlen(target);
    if (n >= sizeof(parent)) {
        errno = ENAMETOOLONG;
        return die("target too long");
    }
    memcpy(parent, target, n + 1);
    char *slash = strrchr(parent, '/');
    if (slash && slash != parent) {
        *slash = '\0';
        if (mkdir_p(parent) < 0) return die("mkdir parent");
    }

    /* Target. */
    struct stat st;
    if (stat(source, &st) < 0) return die("stat source");
    if (S_ISDIR(st.st_mode)) {
        if (mkdir(target, 0755) < 0 && errno != EEXIST) return die("mkdir target");
    } else {
        int fd = open(target, O_CREAT | O_WRONLY | O_CLOEXEC, 0644);
        if (fd < 0 && errno != EEXIST) return die("touch target");
        if (fd >= 0) close(fd);
    }

    if (mount(source, target, NULL, MS_BIND | MS_REC, NULL) < 0) {
        return die("bind mount");
    }
    if (readonly) {
        if (mount(NULL, target, NULL, MS_REMOUNT | MS_BIND | MS_RDONLY, NULL) < 0) {
            int saved = errno;
            (void)umount2(target, MNT_DETACH);
            errno = saved;
            return die("remount ro");
        }
    }
    return 0;
}

static int do_umount(int argc, char *argv[]) {
    if (argc < 4) {
        fprintf(stderr, "nsmount: umount requires TARGET\n");
        return 1;
    }
    const char *target = argv[3];
    if (umount(target) < 0) {
        if (errno == EINVAL || errno == ENOENT) return 0;
        return die("umount");
    }
    return 0;
}

int main(int argc, char *argv[]) {
    if (argc < 3) {
        fprintf(stderr,
                "usage:\n"
                "  %s mount  PID SOURCE TARGET [ro]\n"
                "  %s umount PID TARGET\n",
                argv[0], argv[0]);
        return 1;
    }
    if (enter_ns(argv[2]) < 0) return 1;
    if (strcmp(argv[1], "mount") == 0)  return do_mount(argc, argv);
    if (strcmp(argv[1], "umount") == 0) return do_umount(argc, argv);
    fprintf(stderr, "nsmount: unknown verb %s\n", argv[1]);
    return 1;
}
