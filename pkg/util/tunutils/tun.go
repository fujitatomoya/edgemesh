package cni

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/songgao/water"
	"k8s.io/klog/v2"
)

const (
	tunDevice   = "/dev/net/tun"
	ifnameSize  = 16
	packetSize  = 65536
	ReceiveSize = 50
	SendSize    = 50
)

var (
	once   sync.Once
	tunDev *water.Interface
	err    error
)

var _ net.Conn = (*TunConn)(nil)

type TunConn struct {
	// tunName
	tunName string

	// Tun Interface to handle the tun device
	tun *water.Interface

	// Receive pipeline for transport data to p2p
	ReceivePipe chan []byte

	// Tcp pipeline for transport data to p2p
	WritePipe chan []byte

	// Raw Socket file description
	fd int
}

// Get tun instance once ,if created then return the exist-one
func getTun(name string) (*water.Interface, error) {
	if tunDev == nil {
		once.Do(
			func() {
				tunDev, err = water.New(water.Config{
					DeviceType: water.TUN,
					PlatformSpecificParams: water.PlatformSpecificParams{
						Name: name,
					},
				})
				if err != nil {
					klog.Errorf("create TunInterface failed:", err)
				}
				klog.Infof("Create TunInterface: %s", name)
				err = ExecCommand(fmt.Sprintf("ip link set dev %s up", name))
				if err != nil {
					return
				}
				klog.Infof("set dev %s up succeed", name)
			})
	} else {
		klog.Infof("already create TunInterface")
	}
	return tunDev, err
}

// NewTunConn create fd and calls tun device
func NewTunConn(name string) (*TunConn, error) {
	tun, err := getTun(name)
	if err != nil {
		klog.Errorf("Get TunInterface failed:", err)
		return nil, err
	}

	// create raw socket for communication
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		klog.Errorf("failed to create raw socket", err)
		return nil, err
	}

	klog.Infof("Tun Interface Name: %s\n", name)
	return &TunConn{
		tunName:     name,
		tun:         tun,
		ReceivePipe: make(chan []byte, ReceiveSize),
		WritePipe:   make(chan []byte, SendSize),
		fd:          fd,
	}, nil
}

// Setup Tun Device
// TODO: setup tun device by code but not command  and need to check wether there is a tun
func SetupTunDevice(name string) error {
	err = ExecCommand(fmt.Sprintf("ip link set dev %s up", name))
	if err != nil {
		return err
	}
	klog.Infof("set dev %s up succeed", name)
	return nil
}

func ExecCommand(command string) error {
	cmd := exec.Command("/bin/sh", "-c", command)
	err := cmd.Run()
	output, _ := cmd.Output()
	if err != nil {
		klog.Errorf("failed to execute Command %s , err: %s", command, string(output), err)
		return err
	}
	// check if cmd goes well
	if state := cmd.ProcessState; !state.Success() {
		klog.Errorf("exec command '%s' failed, code=%d, err is ", command, state.ExitCode(), string(output), err)
		return err
	}
	return nil
}

// AddRouteToTun route data to tun device , witch dst IP belongs to  the cidr
func AddRouteToTun(cidr string, name string) error {
	err := ExecCommand(fmt.Sprintf("ip route add  %s dev %s", cidr, name))
	if err != nil {
		return err
	}
	klog.Infof("ip route add table main %s dev %s succeed", cidr, name)
	return nil
}

// CleanTunDevice delete all the Route and change iin kernel
func (tun *TunConn) CleanTunDevice() error {
	err := ExecCommand(fmt.Sprintf("ip link del dev %s mode tun", tun.tunName))
	if err != nil {
		klog.Errorf("Delete Tun Device  failed", err)
		return err
	}
	klog.Infof("Set dev %s down\n", tun.tunName)
	return nil
}

// CleanTunRoute Delete All Routes attach to Tun
func (tun *TunConn) CleanTunRoute(name string) error {
	err := ExecCommand(fmt.Sprintf("ip route flush dev %s", tun.tunName))
	if err != nil {
		klog.Errorf("Delete Tun Route  failed", err)
		return err
	}
	klog.Infof("Removed route from dev %s\n", tun.tunName)
	return nil
}

// CleanSingleTunRoute Delete Single Route attach to Tun
func (tun *TunConn) CleanSingleTunRoute(cidr string) error {
	err := ExecCommand(fmt.Sprintf("ip route del table main %s dev %s", cidr, tun.tunName))
	if err != nil {
		klog.Errorf("Delete Tun Route  failed", err)
		return err
	}
	klog.Infof("Removed route for %s from dev %s\n", cidr, tun.tunName)
	return nil
}

// Read Packet From TunConn. It is designed to Read data from TunConn RecievePipe Channel
// data flow as : tun ---data---> tunConn.ReceivePipe ---Read(cni)---> cni
// when you need to directly read from tun ,you could try tunConn.tun.Read(b []byte)
func (tun *TunConn) Read(packet []byte) (int, error) {
	select {
	case data := <-tun.ReceivePipe:
		if data == nil {
			return 0, io.EOF
		}
		if len(data) > 65535 {
			klog.Error("data length exceeds the maximum allowed size", err)
			return 0, err
		}
		// put data into cni
		copy(packet, data)
		return len(data), nil
	default:
		//receive no data
		return 0, nil
	}
}

// Write Packet To TunConn. It is designed to Write data to TunConn WritePipe Channel
// data flow as : tun <---data--- tunConn.WritePipe <---Write(cni)--- cni
// when you need to directly write to tun ,you could try tunConn.tun.Write(b []byte)
func (tun *TunConn) Write(packet []byte) (int, error) {
	var err error
	n := len(packet)
	if n == 0 {
		klog.Error("Write none to TunConn", err)
		return 0, err
	}
	if n > 65535 {
		klog.Error("cni length exceeds the maximum allowed size", err)
		return n, err
	}
	select {
	case tun.WritePipe <- packet:
		return n, nil
	default:
		klog.Error("Failed to write cni to WritePipe channel", err)
		return 0, err
	}
}

// TunReceiveLoop Acting as Accept(). It will listen to tun, when any cni Read from  tun ，this cni will be read into ReceivePipe
// data flow as : tun ---TunReceiveLoop()---> tunConn.ReceivePipe
// TunReceiveLoop works like producer and Read acts as consumer, when TunConn start, ReceiveLoop should also start to listen to tun
func (tun *TunConn) TunReceiveLoop() {
	// buffer to receive data
	// TODO: add SetReadDeadline implement
	// buffer to receive data
	buffer := NewRecycleByteBuffer(65536)
	packet := make([]byte, 65536)
	for {
		// read from tun Dev
		n, err := tun.tun.Read(packet)
		if err != nil {
			klog.Error("failed to read data from tun", err)
			break
		}
		// get data from tun
		buffer.Write(packet[:n])
		for {
			// Get IP frame to byte data to encapsulate
			frame, err := ParseIPFrame(buffer)
			if err != nil {
				klog.Errorf("Parse frame failed:", err)
				// TODO: should not throw other packets
				buffer.Clean()
				break
			}
			if frame == nil {
				break
			}
			// transfer data to libP2P
			tun.ReceivePipe <- frame.ToBytes()
			// print out the reception data
			klog.Infof("receive from tun, send through tunnel , source %s target %s len %d", frame.GetSourceIP(), frame.GetTargetIP(), frame.GetPayloadLen())
		}
	}
}

// TunWriteLoop  Acting as Dial(). It will "dial" tun, when any cni Write into WritePipe ，this cni will be Write into tun in the form of rae socket
// data flow as : tun <---TunWriteLoop()--- tunConn.WriteLoop
// TunWriteLoop()  works like consumer and Write acts as producer, when TunConn start, WriteLoop should also start to dial tun
func (tun *TunConn) TunWriteLoop() {
	// buffer to write data
	buffer := NewRecycleByteBuffer(65536)
	packet := make([]byte, 65536)
	for {
		//tun.TcpReceivePipe <- frame.ToBytes()
		packet = <-tun.WritePipe
		n := len(packet)
		if n == 0 {
			klog.Error("TunWriteLoop get empty cni ,can not write into tun")
		}
		buffer.Write(packet[:n])
		for {
			// get IP data inside
			frame, err := ParseIPFrame(buffer)
			if err != nil {
				klog.Errorf("failed to parse ip package from WritePipe", err)
			}

			if err != nil {
				klog.Errorf("TunWriteLoop Parse frame failed:", err)
				buffer.Clean()
				break
			}
			if frame == nil {
				break
			}
			klog.Infof("receive from WritePipe, send through raw socket, source %s target %s len %d", frame.GetSourceIP(), frame.GetTargetIP(),
				frame.GetPayloadLen())
			// send ip frame through raw socket
			addr := syscall.SockaddrInet4{
				Addr: IPToArray4(frame.Target),
			}
			// directly send to that IP
			err = syscall.Sendto(tun.fd, frame.ToBytes(), 0, &addr)
			if err != nil {
				klog.Errorf("failed to send data through raw socket", err)
			}
		}
	}
}

func (tun *TunConn) Close() error {
	err := tun.tun.Close()
	if err != nil {
		klog.Errorf("Close Tun falied", err)
		return err
	}
	return nil
}

// LocalAddr return Local Tun Addr
func (tun *TunConn) LocalAddr() net.Addr { return nil }

func (tun *TunConn) RemoteAddr() net.Addr { return nil }

func (tun *TunConn) SetDeadline(t time.Time) error { return nil }

func (tun *TunConn) SetReadDeadline(t time.Time) error { return nil }

func (tun *TunConn) SetWriteDeadline(t time.Time) error { return nil }

func Accept() (*TunConn, error) { return nil, nil }

func Dial() (*TunConn, error) { return nil, nil }

// DialTun run by P2P host
func DialTun(stream net.Conn, name string) {
	p2p2Tun, err := NewTunConn(name)
	if err != nil {
		klog.Errorf("p2p handler create TunConn failed", err)
	}
	packet := make([]byte, packetSize)
	buffer := NewRecycleByteBuffer(packetSize)
	// TODO: separate below as P2P handler and add SetWriteDeadline
	go func() {
		for {
			n, err := stream.Read(packet)
			if err != nil {
				klog.Errorf("Read Data From LibP2P Tunnel failed", err)
				break
			}
			buffer.Write(packet[:n])
			// get IP from cni and send it to TUN
			for {
				frame, err := ParseIPFrame(buffer)
				if err != nil {
					klog.Errorf("failed to parse ip package from tcp tunnel", err)
				}

				if err != nil {
					klog.Errorf("P2P2TUN connection Parse frame failed:", err)
					buffer.Clean()
					break
				}
				if frame == nil {
					break
				}
				klog.Infof("receive from LibP2P, send through raw socket, source %s target %s len %d", frame.GetSourceIP(), frame.GetTargetIP(),
					frame.GetPayloadLen())
				// send ip frame through raw socket
				addr := syscall.SockaddrInet4{
					Addr: IPToArray4(frame.Target),
				}
				// directly send to that IP
				err = syscall.Sendto(p2p2Tun.fd, frame.ToBytes(), 0, &addr)
				if err != nil {
					klog.Errorf("failed to send data through raw socket", err)
				}
			}
		}
	}()
}
