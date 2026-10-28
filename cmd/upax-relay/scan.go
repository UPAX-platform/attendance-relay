package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/UPAX-platform/zk-relay/internal/zk"
)

type found struct {
	addr, serial string
}

func cmdScan(args []string) error {
	fs, _ := newFlags("scan")
	subnet := fs.String("subnet", "", "CIDR to scan, e.g. 192.168.1.0/24 (default: this computer's networks)")
	_ = fs.Parse(args)
	res := scanNetwork(context.Background(), *subnet, true)
	if len(res) == 0 {
		fmt.Println("no devices found")
		return nil
	}
	fmt.Println()
	for _, f := range res {
		fmt.Printf("%-21s %s\n", f.addr, f.serial)
	}
	return nil
}

// localSubnets returns the /24 (or smaller) IPv4 networks this machine is on.
func localSubnets() []*net.IPNet {
	var out []*net.IPNet
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if ones < 24 { // don't sweep huge networks; take the /24 we're in
				ones = 24
			}
			mask := net.CIDRMask(ones, 32)
			out = append(out, &net.IPNet{IP: ipn.IP.To4().Mask(mask), Mask: mask})
		}
	}
	return out
}

func hosts(n *net.IPNet) []net.IP {
	ones, bits := n.Mask.Size()
	if bits != 32 || ones < 16 {
		return nil
	}
	base := binary.BigEndian.Uint32(n.IP.To4())
	count := uint32(1) << (32 - ones)
	var out []net.IP
	for i := uint32(1); i < count-1; i++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, base+i)
		out = append(out, ip)
	}
	return out
}

func scanNetwork(ctx context.Context, cidr string, verbose bool) []found {
	var nets []*net.IPNet
	if cidr != "" {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			fmt.Println("invalid subnet:", err)
			return nil
		}
		nets = append(nets, n)
	} else {
		nets = localSubnets()
	}
	var ips []net.IP
	for _, n := range nets {
		if verbose {
			fmt.Printf("scanning %s for ZKTeco devices (port 4370) ...\n", n)
		}
		ips = append(ips, hosts(n)...)
	}

	var (
		mu  sync.Mutex
		res []found
		wg  sync.WaitGroup
		sem = make(chan struct{}, 128)
	)
	for _, ip := range ips {
		addr := net.JoinHostPort(ip.String(), "4370")
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
			if err != nil {
				return
			}
			conn.Close()
			f := found{addr: ip.String(), serial: "(port open)"}
			if c, err := zk.Dial(ctx, addr, 0, 3*time.Second); err == nil {
				if sn, err := c.SerialNumber(); err == nil {
					f.serial = "serial " + sn
				}
				c.Close()
			} else if err == zk.ErrUnauthorized {
				f.serial = "(ZKTeco, comm key set)"
			}
			mu.Lock()
			res = append(res, f)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(res, func(i, j int) bool { return res[i].addr < res[j].addr })
	if verbose {
		fmt.Printf("found %d device(s)\n", len(res))
	}
	return res
}
