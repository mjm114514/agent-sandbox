package tap

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ifnamsiz   = 16
	tunSetIff  = 0x400454ca
	iffTap     = 0x0002
	iffNoPi    = 0x1000
	defaultMTU = 1500
)

type ifreq struct {
	name  [ifnamsiz]byte
	flags uint16
	_     [22]byte
}

type Bridge struct {
	tapFile *os.File
	name    string
}

func New(name string) (*Bridge, error) {
	// Ensure tun module is loaded
	exec.Command("modprobe", "tun").Run()

	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}

	var req ifreq
	copy(req.name[:], name)
	req.flags = iffTap | iffNoPi

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetIff, uintptr(unsafe.Pointer(&req))); errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("ioctl TUNSETIFF: %w", errno)
	}

	tapFile := os.NewFile(uintptr(fd), "/dev/net/tun")
	b := &Bridge{tapFile: tapFile, name: name}

	if err := b.configureInterface(); err != nil {
		tapFile.Close()
		return nil, err
	}

	return b, nil
}

func (b *Bridge) configureInterface() error {
	cmds := [][]string{
		{"ip", "addr", "add", "10.0.2.1/24", "dev", b.name},
		{"ip", "link", "set", b.name, "up"},
		{"ip", "route", "add", "default", "via", "10.0.2.1", "dev", b.name},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s: %w", args[0], out, err)
		}
	}
	return nil
}

func (b *Bridge) RunTunnel(vsock io.ReadWriteCloser) error {
	var wg sync.WaitGroup
	wg.Add(2)

	errCh := make(chan error, 2)

	// TAP → vsock
	go func() {
		defer wg.Done()
		buf := make([]byte, defaultMTU+64)
		for {
			n, err := b.tapFile.Read(buf)
			if err != nil {
				errCh <- fmt.Errorf("tap read: %w", err)
				return
			}
			frame := buf[:n]
			header := make([]byte, 4)
			binary.BigEndian.PutUint32(header, uint32(n))
			if _, err := vsock.Write(header); err != nil {
				errCh <- fmt.Errorf("vsock write header: %w", err)
				return
			}
			if _, err := vsock.Write(frame); err != nil {
				errCh <- fmt.Errorf("vsock write frame: %w", err)
				return
			}
		}
	}()

	// vsock → TAP
	go func() {
		defer wg.Done()
		header := make([]byte, 4)
		for {
			if _, err := io.ReadFull(vsock, header); err != nil {
				errCh <- fmt.Errorf("vsock read header: %w", err)
				return
			}
			length := binary.BigEndian.Uint32(header)
			frame := make([]byte, length)
			if _, err := io.ReadFull(vsock, frame); err != nil {
				errCh <- fmt.Errorf("vsock read frame: %w", err)
				return
			}
			if _, err := b.tapFile.Write(frame); err != nil {
				errCh <- fmt.Errorf("tap write: %w", err)
				return
			}
		}
	}()

	err := <-errCh
	vsock.Close()
	b.tapFile.Close()
	wg.Wait()
	return err
}

func (b *Bridge) Close() error {
	return b.tapFile.Close()
}
