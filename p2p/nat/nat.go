// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package nat provides access to common network port mapping protocols.
package nat

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/BerithFoundation/berith-chain/log"
	natpmp "github.com/jackpal/go-nat-pmp"
)

// An implementation of nat.Interface can map local ports to ports
// accessible from the Internet.
// nat.Interface의 구현은 인터넷으로부터 포트와 포트끼리 접근 가능하도록 있게 한다.
type Interface interface {
	// These methods manage a mapping between a port on the local
	// machine to a port that can be connected to from the internet.
	// 이 메서드는 로컬머신의 포트와 인터넷으로 부터 연결될 수 있는 포트 사이의 매핑을 관리한다.
	//
	// protocol is "UDP" or "TCP". Some implementations allow setting
	// a display name for the mapping. The mapping may be removed by
	// the gateway when its lifetime ends.
	// 프로토콜은 UDP와 TCP를 사용하며 어떤 구현들은 매핑을 위해 이름을 공개하는 것을 허용한다.
	// 매핑은 게이트웨이의 수명이 다하면 지워질것이다.
	AddMapping(protocol string, extport, intport int, name string, lifetime time.Duration) error
	DeleteMapping(protocol string, extport, intport int) error

	// This method should return the external (Internet-facing)
	// address of the gateway device.
	// 이 메서드는 외부의 게이트웨이 디바이스 주소를 리턴해야만 한다.
	ExternalIP() (net.IP, error)

	// Should return name of the method. This is used for logging.
	String() string
}

// Parse parses a NAT interface description.
// The following formats are currently accepted.
// Note that mechanism names are not case-sensitive.
//
//     "" or "none"         return nil
//     "extip:77.12.33.4"   will assume the local machine is reachable on the given IP
//     "any"                uses the first auto-detected mechanism
//     "upnp"               uses the Universal Plug and Play protocol
//     "pmp"                uses NAT-PMP with an auto-detected gateway address
//     "pmp:192.168.0.1"    uses NAT-PMP with the given gateway address
func Parse(spec string) (Interface, error) {
	var (
		parts = strings.SplitN(spec, ":", 2)
		mech  = strings.ToLower(parts[0])
		ip    net.IP
	)
	if len(parts) > 1 {
		ip = net.ParseIP(parts[1])
		if ip == nil {
			return nil, errors.New("invalid IP address")
		}
	}
	switch mech {
	case "", "none", "off":
		return nil, nil
	case "any", "auto", "on":
		return Any(), nil
	case "extip", "ip":
		if ip == nil {
			return nil, errors.New("missing IP address")
		}
		return ExtIP(ip), nil
	case "upnp":
		return UPnP(), nil
	case "pmp", "natpmp", "nat-pmp":
		return PMP(ip), nil
	default:
		return nil, fmt.Errorf("unknown mechanism %q", parts[0])
	}
}

const (
	mapTimeout        = 20 * time.Minute
	mapUpdateInterval = 15 * time.Minute
)

// Map adds a port mapping on m and keeps it alive until c is closed.
// This function is typically invoked in its own goroutine.
func Map(m Interface, c chan struct{}, protocol string, extport, intport int, name string) {
	log := log.New("proto", protocol, "extport", extport, "intport", intport, "interface", m)
	refresh := time.NewTimer(mapUpdateInterval)
	defer func() {
		refresh.Stop()
		log.Debug("Deleting port mapping")
		m.DeleteMapping(protocol, extport, intport)
	}()
	if err := m.AddMapping(protocol, extport, intport, name, mapTimeout); err != nil {
		log.Debug("Couldn't add port mapping", "err", err)
	} else {
		log.Info("Mapped network port")
	}
	for {
		select {
		case _, ok := <-c:
			if !ok {
				return
			}
		case <-refresh.C:
			log.Trace("Refreshing port mapping")
			if err := m.AddMapping(protocol, extport, intport, name, mapTimeout); err != nil {
				log.Debug("Couldn't add port mapping", "err", err)
			}
			refresh.Reset(mapUpdateInterval)
		}
	}
}

// ExtIP assumes that the local machine is reachable on the given
// external IP address, and that any required ports were mapped manually.
// Mapping operations will not return an error but won't actually do anything.
type ExtIP net.IP

func (n ExtIP) ExternalIP() (net.IP, error) { return net.IP(n), nil }
func (n ExtIP) String() string              { return fmt.Sprintf("ExtIP(%v)", net.IP(n)) }

// These do nothing.

func (ExtIP) AddMapping(string, int, int, string, time.Duration) error { return nil }
func (ExtIP) DeleteMapping(string, int, int) error                     { return nil }

// Any returns a port mapper that tries to discover any supported
// mechanism on the local network.
func Any() Interface {
	// TODO: attempt to discover whether the local machine has an
	// Internet-class address. Return ExtIP in this case.
	return startautodisc("UPnP or NAT-PMP", func() Interface {
		found := make(chan Interface, 2)
		go func() { found <- discoverUPnP() }()
		go func() { found <- discoverPMP() }()
		for i := 0; i < cap(found); i++ {
			if c := <-found; c != nil {
				return c
			}
		}
		return nil
	})
}

// UPnP returns a port mapper that uses UPnP. It will attempt to
// discover the address of your router using UDP broadcasts.
func UPnP() Interface {
	return startautodisc("UPnP", discoverUPnP)
}

// PMP returns a port mapper that uses NAT-PMP. The provided gateway
// address should be the IP of your router. If the given gateway
// address is nil, PMP will attempt to auto-discover the router.
func PMP(gateway net.IP) Interface {
	if gateway != nil {
		return &pmp{gw: gateway, c: natpmp.NewClient(gateway)}
	}
	return startautodisc("NAT-PMP", discoverPMP)
}

// autodisc represents a port mapping mechanism that is still being
// auto-discovered. Calls to the Interface methods on this type will
// wait until the discovery is done and then call the method on the
// discovered mechanism.
// autodisc는 여전히 자동 탐색중인 포트 매핑 매커니즘을 대변한다.
// 이 타입의 인터페이스 메서드에 대한 호출은 탐색이 종료될 때 까지 대기한 다음
// 검색된 메커니즘의 메서드를 호출한다.
//
// This type is useful because discovery can take a while but we
// want return an Interface value from UPnP, PMP and Auto immediately.
// 이 타입은 탐색에 시간이 걸릴 수 있지만 UPnP, PMP 및 Auto에서 인터페이스 값을 즉시 반환해야 하므로 유용하다.
type autodisc struct {
	what string // type of interface being autodiscovered
	once sync.Once
	doit func() Interface

	mu    sync.Mutex
	found Interface
}

func startautodisc(what string, doit func() Interface) Interface {
	// TODO: monitor network configuration and rerun doit when it changes.
	return &autodisc{what: what, doit: doit}
}

func (n *autodisc) AddMapping(protocol string, extport, intport int, name string, lifetime time.Duration) error {
	if err := n.wait(); err != nil {
		return err
	}
	return n.found.AddMapping(protocol, extport, intport, name, lifetime)
}

func (n *autodisc) DeleteMapping(protocol string, extport, intport int) error {
	if err := n.wait(); err != nil {
		return err
	}
	return n.found.DeleteMapping(protocol, extport, intport)
}

func (n *autodisc) ExternalIP() (net.IP, error) {
	if err := n.wait(); err != nil {
		return nil, err
	}
	return n.found.ExternalIP()
}

func (n *autodisc) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.found == nil {
		return n.what
	} else {
		return n.found.String()
	}
}

// wait blocks until auto-discovery has been performed.
func (n *autodisc) wait() error {
	n.once.Do(func() {
		n.mu.Lock()
		n.found = n.doit()
		n.mu.Unlock()
	})
	if n.found == nil {
		return fmt.Errorf("no %s router discovered", n.what)
	}
	return nil
}
