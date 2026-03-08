//go:build linux

package connkill

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// nativeEndian is the byte order of the current platform.
var nativeEndian binary.ByteOrder

func init() {
	buf := [2]byte{}
	*(*uint16)(unsafe.Pointer(&buf[0])) = 0x0102
	if buf[0] == 0x01 {
		nativeEndian = binary.BigEndian
	} else {
		nativeEndian = binary.LittleEndian
	}
}

// Netlink constants for SOCK_DIAG / SOCK_DESTROY.
const (
	SOCK_DIAG_BY_FAMILY = 20
	SOCK_DESTROY        = 21

	// TCP states bitmask (all states).
	tcpAllStates = 0xFFF

	NETLINK_SOCK_DIAG = 4
)

// inetDiagSockID is the socket identity structure.
type inetDiagSockID struct {
	SPort  [2]byte  // source port (network byte order)
	DPort  [2]byte  // destination port (network byte order)
	Src    [16]byte // source address
	Dst    [16]byte // destination address
	If     uint32
	Cookie [2]uint32
}

// inetDiagReqV2 is the request structure for SOCK_DIAG_BY_FAMILY.
type inetDiagReqV2 struct {
	Family   uint8
	Protocol uint8
	Ext      uint8
	Pad      uint8
	States   uint32
	ID       inetDiagSockID
}

// inetDiagMsg is the response message from SOCK_DIAG.
type inetDiagMsg struct {
	Family  uint8
	State   uint8
	Timer   uint8
	Retrans uint8
	ID      inetDiagSockID
	Expires uint32
	RQueue  uint32
	WQueue  uint32
	UID     uint32
	Inode   uint32
}

// DestroySocketsByDestIP destroys all TCP connections to the given destination IP.
// Returns the number of connections destroyed and the number of per-socket destroy errors.
func DestroySocketsByDestIP(destIP net.IP) (destroyed, errCount int, err error) {
	family := uint8(unix.AF_INET)
	if destIP.To4() == nil {
		family = uint8(unix.AF_INET6)
	}

	// Open NETLINK_SOCK_DIAG socket for dump
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM, NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, 0, fmt.Errorf("opening netlink socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(fd, sa); err != nil {
		return 0, 0, fmt.Errorf("binding netlink socket: %w", err)
	}

	// Phase 1: Collect all matching sockets from the dump.
	// We must NOT interleave destroy requests on the same FD during the dump,
	// because destroy ACK reads would consume dump messages (and vice versa).
	var targets []inetDiagSockID
	if err := forEachTCPSocket(fd, family, func(msg *inetDiagMsg) bool {
		if matchesDest(*msg, destIP, family) {
			targets = append(targets, msg.ID)
		}
		return true
	}); err != nil {
		return 0, 0, fmt.Errorf("iterating TCP sockets: %w", err)
	}

	// Phase 2: Destroy collected sockets. Now the dump is complete,
	// so reads on this FD will only receive destroy ACKs.
	for i := range targets {
		msg := &inetDiagMsg{ID: targets[i]}
		if err := destroySocket(fd, family, msg); err != nil {
			errCount++
		} else {
			destroyed++
		}
	}

	return destroyed, errCount, nil
}

// forEachTCPSocket streams TCP socket diagnostic messages from netlink, invoking fn for each.
// If fn returns false, iteration stops early. This avoids accumulating all sockets in memory.
func forEachTCPSocket(fd int, family uint8, fn func(*inetDiagMsg) bool) error {
	req := inetDiagReqV2{
		Family:   family,
		Protocol: unix.IPPROTO_TCP,
		States:   tcpAllStates,
	}

	nlmsgLen := syscall.NLMSG_HDRLEN + int(unsafe.Sizeof(req))
	buf := make([]byte, nlmsgLen)

	// NLM header
	nativeEndian.PutUint32(buf[0:4], uint32(nlmsgLen))
	nativeEndian.PutUint16(buf[4:6], SOCK_DIAG_BY_FAMILY)
	nativeEndian.PutUint16(buf[6:8], syscall.NLM_F_DUMP|syscall.NLM_F_REQUEST)
	nativeEndian.PutUint32(buf[8:12], 1)  // Seq
	nativeEndian.PutUint32(buf[12:16], 0) // PID

	reqBytes := (*[unsafe.Sizeof(req)]byte)(unsafe.Pointer(&req))
	copy(buf[syscall.NLMSG_HDRLEN:], reqBytes[:])

	if _, err := unix.Write(fd, buf); err != nil {
		return fmt.Errorf("sending diag request: %w", err)
	}

	// Set a 10-second receive timeout to prevent indefinite blocking
	tv := unix.Timeval{Sec: 10}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("setting recv timeout: %w", err)
	}

	recvBuf := make([]byte, 65536)

	for {
		n, _, err := unix.Recvfrom(fd, recvBuf, 0)
		if err != nil {
			return fmt.Errorf("recvfrom: %w", err)
		}

		msgs, err := syscall.ParseNetlinkMessage(recvBuf[:n])
		if err != nil {
			return fmt.Errorf("parsing netlink messages: %w", err)
		}

		done := false
		for _, m := range msgs {
			if m.Header.Type == syscall.NLMSG_DONE {
				done = true
				break
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				if len(m.Data) >= 4 {
					errno := int32(nativeEndian.Uint32(m.Data[0:4]))
					if errno != 0 {
						return fmt.Errorf("netlink error: %d", errno)
					}
				}
				continue
			}

			if len(m.Data) >= int(unsafe.Sizeof(inetDiagMsg{})) {
				var msg inetDiagMsg
				msgBytes := (*[unsafe.Sizeof(msg)]byte)(unsafe.Pointer(&msg))
				copy(msgBytes[:], m.Data)
				if !fn(&msg) {
					return nil
				}
			}
		}

		if done {
			break
		}
	}

	return nil
}

func matchesDest(msg inetDiagMsg, destIP net.IP, family uint8) bool {
	if family == unix.AF_INET {
		ip4 := destIP.To4()
		if ip4 == nil {
			return false
		}
		return msg.ID.Dst[0] == ip4[0] && msg.ID.Dst[1] == ip4[1] &&
			msg.ID.Dst[2] == ip4[2] && msg.ID.Dst[3] == ip4[3]
	}
	ip6 := destIP.To16()
	if ip6 == nil {
		return false
	}
	for i := range 16 {
		if msg.ID.Dst[i] != ip6[i] {
			return false
		}
	}
	return true
}

func destroySocket(fd int, family uint8, msg *inetDiagMsg) error {
	req := inetDiagReqV2{
		Family:   family,
		Protocol: unix.IPPROTO_TCP,
		States:   tcpAllStates,
		ID:       msg.ID,
	}

	nlmsgLen := syscall.NLMSG_HDRLEN + int(unsafe.Sizeof(req))
	buf := make([]byte, nlmsgLen)

	nativeEndian.PutUint32(buf[0:4], uint32(nlmsgLen))
	nativeEndian.PutUint16(buf[4:6], SOCK_DESTROY)
	nativeEndian.PutUint16(buf[6:8], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	nativeEndian.PutUint32(buf[8:12], 2)
	nativeEndian.PutUint32(buf[12:16], 0)

	reqBytes := (*[unsafe.Sizeof(req)]byte)(unsafe.Pointer(&req))
	copy(buf[syscall.NLMSG_HDRLEN:], reqBytes[:])

	if _, err := unix.Write(fd, buf); err != nil {
		return fmt.Errorf("sending destroy request: %w", err)
	}

	// Set a 5-second receive timeout to prevent indefinite blocking
	tv := unix.Timeval{Sec: 5}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("setting destroy recv timeout: %w", err)
	}

	ackBuf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, ackBuf, 0)
	if err != nil {
		return fmt.Errorf("reading destroy ack: %w", err)
	}

	msgs, err := syscall.ParseNetlinkMessage(ackBuf[:n])
	if err != nil {
		return fmt.Errorf("parsing destroy ack: %w", err)
	}

	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR {
			if len(m.Data) >= 4 {
				errno := int32(nativeEndian.Uint32(m.Data[0:4]))
				if errno != 0 {
					return fmt.Errorf("destroy error: %d", errno)
				}
			}
		}
	}

	return nil
}
