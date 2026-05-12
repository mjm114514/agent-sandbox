#ifndef AVF_BRIDGE_DARWIN_H
#define AVF_BRIDGE_DARWIN_H

#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef void *avf_vm_t;
typedef void *avf_listener_t;

typedef struct {
    const char *kernel_path;
    const char *initrd_path;       // empty string => no initrd
    const char *cmdline;
    const char *rootfs_path;       // read/write raw disk image
    const char *guestpack_path;    // empty string => no guestpack disk
    int vcpus;
    uint64_t memory_bytes;
} avf_vm_config_t;

// Create the VM. Returns NULL on error and writes an allocated error message
// to *err_out (caller frees via avf_free_str).
avf_vm_t avf_vm_create(const avf_vm_config_t *cfg, char **err_out);

// Start / stop / destroy. Start and Stop block until the framework's
// completion handler fires. Destroy releases the VM object (call Stop first).
int avf_vm_start(avf_vm_t vm, char **err_out);
int avf_vm_stop(avf_vm_t vm, char **err_out);
void avf_vm_destroy(avf_vm_t vm);

// Register a virtio-vsock listener on the given guest-side port. Returns an
// opaque listener handle whose lifetime is tied to the owning VM. Failed
// registration returns NULL with *err_out set.
avf_listener_t avf_vm_listen(avf_vm_t vm, uint32_t port, char **err_out);

// Block until a new vsock connection arrives, returning a dup'd file
// descriptor (caller closes). Returns -1 once the listener is closed.
int avf_listener_accept(avf_listener_t lis);

// Close the listener: wake any pending accept() with -1, drain any
// in-flight accept calls, and release the cgo-side strong ref handed back
// by avf_vm_listen. After this returns the handle is invalid.
void avf_listener_close(avf_listener_t lis);

// Remove the framework's listener registration for `port` and release the
// owning VM's strong reference. Used when re-VSockListen-ing a port whose
// prior listener has been closed.
void avf_vm_unlisten(avf_vm_t vm, uint32_t port);

void avf_free_str(char *s);

#ifdef __cplusplus
}
#endif

#endif
