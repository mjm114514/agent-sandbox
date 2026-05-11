#import <Foundation/Foundation.h>
#import <Virtualization/Virtualization.h>
#import <unistd.h>

#import "bridge_darwin.h"

API_AVAILABLE(macos(12.0))
@interface AVFListener : NSObject <VZVirtioSocketListenerDelegate>
@property (nonatomic, strong) NSMutableArray<NSNumber *> *pendingFDs;
@property (nonatomic, strong) NSCondition *cond;
@property (nonatomic, assign) BOOL closed;
- (BOOL)listener:(VZVirtioSocketListener *)listener
        shouldAcceptNewConnection:(VZVirtioSocketConnection *)conn
        fromSocketDevice:(VZVirtioSocketDevice *)dev;
- (int)acceptBlocking;
- (void)closeListener;
@end

@implementation AVFListener
- (instancetype)init {
    if ((self = [super init])) {
        _pendingFDs = [NSMutableArray array];
        _cond = [[NSCondition alloc] init];
        _closed = NO;
    }
    return self;
}

- (BOOL)listener:(VZVirtioSocketListener *)listener
        shouldAcceptNewConnection:(VZVirtioSocketConnection *)conn
        fromSocketDevice:(VZVirtioSocketDevice *)dev {
    // The connection's fileDescriptor is owned by the framework; dup so the
    // FD survives this connection object being released by ARC.
    int fd = dup(conn.fileDescriptor);
    if (fd < 0) {
        return NO;
    }
    [_cond lock];
    if (_closed) {
        [_cond unlock];
        close(fd);
        return NO;
    }
    [_pendingFDs addObject:@(fd)];
    [_cond signal];
    [_cond unlock];
    return YES;
}

- (int)acceptBlocking {
    [_cond lock];
    while (_pendingFDs.count == 0 && !_closed) {
        [_cond wait];
    }
    int fd = -1;
    if (_pendingFDs.count > 0) {
        fd = [_pendingFDs.firstObject intValue];
        [_pendingFDs removeObjectAtIndex:0];
    }
    [_cond unlock];
    return fd;
}

- (void)closeListener {
    [_cond lock];
    _closed = YES;
    for (NSNumber *n in _pendingFDs) {
        close([n intValue]);
    }
    [_pendingFDs removeAllObjects];
    [_cond broadcast];
    [_cond unlock];
}
@end

API_AVAILABLE(macos(12.0))
@interface AVFVM : NSObject
@property (nonatomic, strong) VZVirtualMachine *vm;
@property (nonatomic, strong) dispatch_queue_t queue;
// port -> AVFListener / VZVirtioSocketListener kept alive while the VM lives.
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, AVFListener *> *listeners;
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, VZVirtioSocketListener *> *vzListeners;
@end

@implementation AVFVM
- (instancetype)init {
    if ((self = [super init])) {
        _queue = dispatch_queue_create("com.anthropic.agent-sandbox.avf", DISPATCH_QUEUE_SERIAL);
        _listeners = [NSMutableDictionary dictionary];
        _vzListeners = [NSMutableDictionary dictionary];
    }
    return self;
}
@end

static char *copy_err(NSString *s) {
    const char *cs = s ? [s UTF8String] : "unknown error";
    return strdup(cs ? cs : "unknown error");
}

avf_vm_t avf_vm_create(const avf_vm_config_t *cfg, char **err_out) {
    if (@available(macOS 12.0, *)) {
        @autoreleasepool {
            NSError *err = nil;
            VZVirtualMachineConfiguration *config = [[VZVirtualMachineConfiguration alloc] init];

            // Linux boot loader (kernel + optional initrd + cmdline)
            NSURL *kernelURL = [NSURL fileURLWithPath:[NSString stringWithUTF8String:cfg->kernel_path]];
            VZLinuxBootLoader *boot = [[VZLinuxBootLoader alloc] initWithKernelURL:kernelURL];
            if (cfg->initrd_path && cfg->initrd_path[0]) {
                boot.initialRamdiskURL = [NSURL fileURLWithPath:[NSString stringWithUTF8String:cfg->initrd_path]];
            }
            if (cfg->cmdline && cfg->cmdline[0]) {
                boot.commandLine = [NSString stringWithUTF8String:cfg->cmdline];
            }
            config.bootLoader = boot;

            // CPU & memory. AVF takes bytes for memory.
            config.CPUCount = cfg->vcpus > 0 ? cfg->vcpus : 4;
            config.memorySize = cfg->memory_bytes > 0 ? cfg->memory_bytes : (8ULL * 1024 * 1024 * 1024);

            // Storage: rootfs (rw) at vda, optional guestpack (ro) at vdb.
            NSMutableArray *storage = [NSMutableArray array];
            NSURL *rootURL = [NSURL fileURLWithPath:[NSString stringWithUTF8String:cfg->rootfs_path]];
            VZDiskImageStorageDeviceAttachment *rootAtt =
                [[VZDiskImageStorageDeviceAttachment alloc] initWithURL:rootURL readOnly:NO error:&err];
            if (!rootAtt) {
                if (err_out) *err_out = copy_err([NSString stringWithFormat:@"rootfs attach: %@", err.localizedDescription]);
                return NULL;
            }
            [storage addObject:[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:rootAtt]];

            if (cfg->guestpack_path && cfg->guestpack_path[0]) {
                NSURL *gpURL = [NSURL fileURLWithPath:[NSString stringWithUTF8String:cfg->guestpack_path]];
                VZDiskImageStorageDeviceAttachment *gpAtt =
                    [[VZDiskImageStorageDeviceAttachment alloc] initWithURL:gpURL readOnly:YES error:&err];
                if (!gpAtt) {
                    if (err_out) *err_out = copy_err([NSString stringWithFormat:@"guestpack attach: %@", err.localizedDescription]);
                    return NULL;
                }
                [storage addObject:[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:gpAtt]];
            }
            config.storageDevices = storage;

            // virtio-vsock device. setSocketListener:forPort: is wired
            // post-create via avf_vm_listen.
            VZVirtioSocketDeviceConfiguration *vsockCfg = [[VZVirtioSocketDeviceConfiguration alloc] init];
            config.socketDevices = @[vsockCfg];

            // Serial console -> stderr so kernel boot logs are visible to the
            // operator. Read side is unused.
            VZFileHandleSerialPortAttachment *serialAtt =
                [[VZFileHandleSerialPortAttachment alloc] initWithFileHandleForReading:nil
                                                                   fileHandleForWriting:[NSFileHandle fileHandleWithStandardError]];
            VZVirtioConsoleDeviceSerialPortConfiguration *serial =
                [[VZVirtioConsoleDeviceSerialPortConfiguration alloc] init];
            serial.attachment = serialAtt;
            config.serialPorts = @[serial];

            // Small utility devices.
            config.memoryBalloonDevices = @[[[VZVirtioTraditionalMemoryBalloonDeviceConfiguration alloc] init]];
            config.entropyDevices = @[[[VZVirtioEntropyDeviceConfiguration alloc] init]];

            if (![config validateWithError:&err]) {
                if (err_out) *err_out = copy_err([NSString stringWithFormat:@"validate config: %@", err.localizedDescription]);
                return NULL;
            }

            AVFVM *wrapper = [[AVFVM alloc] init];
            wrapper.vm = [[VZVirtualMachine alloc] initWithConfiguration:config queue:wrapper.queue];
            // Hand a retained reference to the Go side; balanced by
            // __bridge_transfer in avf_vm_destroy.
            return (__bridge_retained void *)wrapper;
        }
    } else {
        if (err_out) *err_out = strdup("Virtualization.framework requires macOS 12 or later");
        return NULL;
    }
}

int avf_vm_start(avf_vm_t handle, char **err_out) {
    if (@available(macOS 12.0, *)) {
        AVFVM *wrapper = (__bridge AVFVM *)handle;
        __block NSError *startErr = nil;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        dispatch_async(wrapper.queue, ^{
            [wrapper.vm startWithCompletionHandler:^(NSError * _Nullable error) {
                if (error) startErr = error;
                dispatch_semaphore_signal(sem);
            }];
        });
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (startErr) {
            if (err_out) *err_out = copy_err([NSString stringWithFormat:@"start: %@", startErr.localizedDescription]);
            return -1;
        }
        return 0;
    }
    if (err_out) *err_out = strdup("macOS 12+ required");
    return -1;
}

int avf_vm_stop(avf_vm_t handle, char **err_out) {
    if (@available(macOS 12.0, *)) {
        AVFVM *wrapper = (__bridge AVFVM *)handle;
        __block NSError *stopErr = nil;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        dispatch_async(wrapper.queue, ^{
            // Forceful stop — we don't rely on an ACPI shutdown path because
            // the guest may not have a working signal handler on a panic.
            [wrapper.vm stopWithCompletionHandler:^(NSError * _Nullable error) {
                if (error) stopErr = error;
                dispatch_semaphore_signal(sem);
            }];
        });
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (stopErr) {
            if (err_out) *err_out = copy_err([NSString stringWithFormat:@"stop: %@", stopErr.localizedDescription]);
            return -1;
        }
        return 0;
    }
    if (err_out) *err_out = strdup("macOS 12+ required");
    return -1;
}

void avf_vm_destroy(avf_vm_t handle) {
    if (@available(macOS 12.0, *)) {
        AVFVM *wrapper = (__bridge_transfer AVFVM *)handle;
        @synchronized (wrapper) {
            for (AVFListener *l in wrapper.listeners.allValues) {
                [l closeListener];
            }
            [wrapper.listeners removeAllObjects];
            [wrapper.vzListeners removeAllObjects];
        }
        wrapper.vm = nil;
    }
}

avf_listener_t avf_vm_listen(avf_vm_t handle, uint32_t port, char **err_out) {
    if (@available(macOS 12.0, *)) {
        AVFVM *wrapper = (__bridge AVFVM *)handle;

        // VZVirtualMachine state must be touched on the VM queue.
        __block VZVirtioSocketDevice *dev = nil;
        dispatch_sync(wrapper.queue, ^{
            if (wrapper.vm.socketDevices.count > 0) {
                dev = (VZVirtioSocketDevice *)wrapper.vm.socketDevices.firstObject;
            }
        });
        if (!dev) {
            if (err_out) *err_out = strdup("VM has no virtio-socket device");
            return NULL;
        }

        AVFListener *l = [[AVFListener alloc] init];
        VZVirtioSocketListener *vzl = [[VZVirtioSocketListener alloc] init];
        vzl.delegate = l;

        dispatch_sync(wrapper.queue, ^{
            [dev setSocketListener:vzl forPort:port];
        });

        @synchronized (wrapper) {
            wrapper.listeners[@(port)] = l;
            wrapper.vzListeners[@(port)] = vzl;
        }
        // Unretained handle: lifetime is bound to wrapper.listeners.
        return (__bridge void *)l;
    }
    if (err_out) *err_out = strdup("macOS 12+ required");
    return NULL;
}

int avf_listener_accept(avf_listener_t handle) {
    if (@available(macOS 12.0, *)) {
        if (!handle) return -1;
        AVFListener *l = (__bridge AVFListener *)handle;
        return [l acceptBlocking];
    }
    return -1;
}

void avf_listener_close(avf_listener_t handle) {
    if (@available(macOS 12.0, *)) {
        if (!handle) return;
        AVFListener *l = (__bridge AVFListener *)handle;
        [l closeListener];
    }
}

void avf_free_str(char *s) {
    if (s) free(s);
}
