package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

var (
	debug           bool
	mtu             int
	endpoints       arrayFlags
	vpnkitSocket    string
	qemuSocket      string
	forwardSocket   string
	forwardDest     string
	forwardUser     string
	forwardIdentify string
	sshPort         int
	pidFile         string
	exitCode        int
)

func main() {
	flag.Var(&endpoints, "listen", fmt.Sprintf("URL where the tap send packets (default %s)", transport.DefaultURL))
	flag.BoolVar(&debug, "debug", false, "Print debug info")
	flag.IntVar(&mtu, "mtu", 1500, "Set the MTU")
	flag.IntVar(&sshPort, "ssh-port", 2222, "Port to access the guest virtual machine. Must be between 1024 and 65535")
	flag.StringVar(&vpnkitSocket, "listen-vpnkit", "", "VPNKit socket to be used by Hyperkit")
	flag.StringVar(&qemuSocket, "listen-qemu", "", "Socket to be used by Qemu")
	flag.StringVar(&forwardSocket, "forward-sock", "", "Forwards a unix socket to the guest virtual machine over SSH")
	flag.StringVar(&forwardDest, "forward-dest", "", "Forwards a unix socket to the guest virtual machine over SSH")
	flag.StringVar(&forwardUser, "forward-user", "", "SSH user to use for unix socket forward")
	flag.StringVar(&forwardIdentify, "forward-identity", "", "Path to SSH identity key for forwarding")
	flag.StringVar(&pidFile, "pid-file", "", "Generate a file with the PID in it")
	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	// Make this the last defer statement in the stack
	defer os.Exit(exitCode)

	groupErrs, ctx := errgroup.WithContext(ctx)
	// Setup signal channel for catching user signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	if debug {
		log.SetLevel(log.DebugLevel)
	}
	if len(endpoints) == 0 {
		endpoints = append(endpoints, transport.DefaultURL)
	}
	// Make sure the qemu socket provided is valid syntax
	if len(qemuSocket) > 0 {
		uri, err := url.Parse(qemuSocket)
		if err != nil || uri == nil {
			exitWithError(errors.Wrapf(err, "invalid value for listen-qemu"))
		}
		if _, err := os.Stat(uri.Path); err == nil && uri.Scheme == "unix" {
			exitWithError(errors.Errorf("%q already exists", uri.Path))
		}
	}
	if vpnkitSocket != "" && qemuSocket != "" {
		exitWithError(errors.New("cannot use qemu and vpnkit protocol at the same time"))
	}
	// If the given port is not between the privileged ports
	// and the oft considered maximum port, return an error.
	if sshPort < 1024 || sshPort > 65535 {
		exitWithError(errors.New("ssh-port value must be between 1024 and 65535"))
	}
	protocol := types.HyperKitProtocol
	if qemuSocket != "" {
		protocol = types.QemuProtocol
	}

	forwardCount := 0
	if forwardDest != "" {
		forwardCount++
	}
	if forwardIdentify != "" {
		_, err := os.Stat(forwardIdentify)
		if err != nil {
			exitWithError(errors.Wrapf(err, "Identity file %s can't be loaded", forwardIdentify))
		}
		forwardCount++
	}
	if forwardUser != "" {
		forwardCount++
	}
	if forwardSocket != "" {
		forwardCount++
	}

	if forwardCount > 0 && forwardCount < 4 {
		exitWithError(errors.New("-forward-sock, --forward-dest, --forward-user, and --forward-identity must all be specified together, or none specified"))
	}

	// Create a PID file if requested
	if len(pidFile) > 0 {
		f, err := os.Create(pidFile)
		if err != nil {
			exitWithError(err)
		}
		// Remove the pid-file when exiting
		defer func() {
			if err := os.Remove(pidFile); err != nil {
				log.Error(err)
			}
		}()
		pid := os.Getpid()
		if _, err := f.WriteString(strconv.Itoa(pid)); err != nil {
			exitWithError(err)
		}
	}

	config := types.Configuration{
		Debug:             debug,
		CaptureFile:       captureFile(),
		MTU:               mtu,
		Subnet:            "192.168.127.0/24",
		GatewayIP:         "192.168.127.1",
		GatewayMacAddress: "5a:94:ef:e4:0c:dd",
		DHCPStaticLeases: map[string]string{
			"192.168.127.2": "5a:94:ef:e4:0c:ee",
		},
		DNS: []types.Zone{
			{
				Name: "containers.internal.",
				Records: []types.Record{
					{
						Name: "gateway",
						IP:   net.ParseIP("192.168.127.1"),
					},
					{
						Name: "host",
						IP:   net.ParseIP("192.168.127.254"),
					},
				},
			},
			{
				Name: "crc.testing.", // still used by current version of podman machine CNI
				Records: []types.Record{
					{
						Name: "gateway",
						IP:   net.ParseIP("192.168.127.1"),
					},
					{
						Name: "host",
						IP:   net.ParseIP("192.168.127.254"),
					},
				},
			},
		},
		Forwards: map[string]string{
			fmt.Sprintf(":%d", sshPort): "192.168.127.2:22",
		},
		NAT: map[string]string{
			"192.168.127.254": "127.0.0.1",
		},
		GatewayVirtualIPs: []string{"192.168.127.254"},
		VpnKitUUIDMacAddresses: map[string]string{
			"c3d68012-0208-11ea-9fd7-f2189899ab08": "5a:94:ef:e4:0c:ee",
		},
		Protocol:    protocol,
		SSHHostPort: "192.168.127.2:22",
	}

	groupErrs.Go(func() error {
		return run(ctx, groupErrs, &config, endpoints)
	})

	// Wait for something to happen
	groupErrs.Go(func() error {
		select {
		// Catch signals so exits are graceful and defers can run
		case <-sigChan:
			cancel()
			return errors.New("signal caught")
		case <-ctx.Done():
			return nil
		}
	})
	// Wait for all of the go funcs to finish up
	if err := groupErrs.Wait(); err != nil {
		log.Error(err)
		exitCode = 1
	}
}

type arrayFlags []string

func (i *arrayFlags) String() string {
	return "my string representation"
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func captureFile() string {
	if !debug {
		return ""
	}
	return "capture.pcap"
}

func run(ctx context.Context, g *errgroup.Group, configuration *types.Configuration, endpoints []string) error {
	vn, err := virtualnetwork.New(configuration)
	if err != nil {
		return err
	}
	log.Info("waiting for clients...")

	for _, endpoint := range endpoints {
		log.Infof("listening %s", endpoint)
		ln, err := transport.Listen(endpoint)
		if err != nil {
			return errors.Wrap(err, "cannot listen")
		}
		g.Go(func() error {
			<-ctx.Done()
			return ln.Close()
		})
		g.Go(func() error {
			err := http.Serve(ln, withProfiler(vn))
			if err != nil {
				if err != http.ErrServerClosed {
					return err
				}
				return err
			}
			return nil
		})
	}
	if debug {
		g.Go(func() error {
		debugLog:
			for {
				select {
				case <-time.After(5 * time.Second):
					fmt.Printf("%v sent to the VM, %v received from the VM\n", humanize.Bytes(vn.BytesSent()), humanize.Bytes(vn.BytesReceived()))
				case <-ctx.Done():
					break debugLog
				}
			}
			return nil
		})
	}

	if vpnkitSocket != "" {
		vpnkitListener, err := transport.Listen(vpnkitSocket)
		if err != nil {
			return err
		}
		g.Go(func() error {
		vpnloop:
			for {
				select {
				case <-ctx.Done():
					break vpnloop
				default:
					// pass through
				}
				conn, err := vpnkitListener.Accept()
				if err != nil {
					log.Errorf("vpnkit accept error: %s", err)
					continue
				}
				g.Go(func() error {
					return vn.AcceptVpnKit(conn)
				})
			}
			return nil
		})
	}

	if qemuSocket != "" {
		qemuListener, err := transport.Listen(qemuSocket)
		if err != nil {
			return err
		}

		g.Go(func() error {
			<-ctx.Done()
			if err := qemuListener.Close(); err != nil {
				log.Errorf("error closing %s: %q", qemuSocket, err)
			}
			return os.Remove(qemuSocket)
		})

		g.Go(func() error {
			conn, err := qemuListener.Accept()
			if err != nil {
				return errors.Wrap(err, "qemu accept error")

			}
			return vn.AcceptQemu(ctx, conn)
		})
	}

	if forwardSocket != "" {
		dest := url.URL{
			Scheme: "ssh",
			User:   url.User(forwardUser),
			Host:   configuration.SSHHostPort,
			Path:   forwardDest,
		}
		g.Go(func() error {
			defer os.Remove(forwardSocket)
			forward, err := virtualnetwork.CreateSSHForward(ctx, forwardSocket, dest, forwardIdentify, vn)
			if err != nil {
				return err
			}
			go func() {
				<-ctx.Done()
				// Abort pending accepts
				forward.Close()
			}()
		loop:
			for {
				select {
				case <-ctx.Done():
					break loop
				default:
					// proceed
				}
				err := forward.AcceptAndTunnel(ctx)
				if err != nil {
					log.Debugf("Error occurred handling ssh forwarded connection: %q", err)
				}
			}
			return nil
		})
	}

	return nil
}

func withProfiler(vn *virtualnetwork.VirtualNetwork) http.Handler {
	mux := vn.Mux()
	if debug {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	}
	return mux
}

func exitWithError(err error) {
	log.Error(err)
	os.Exit(1)
}
