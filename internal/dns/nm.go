// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package dns

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/godbus/dbus/v5"
	"tailscale.com/util/dnsname"
	"tailscale.com/util/endian"
)

const (
	highestPriority = int32(-1 << 31)
	mediumPriority  = int32(1)   // Highest priority that doesn't hard-override
	lowerPriority   = int32(200) // lower than all builtin auto priorities
)

// nmManager uses the NetworkManager DBus API.
type nmManager struct {
	interfaceName string
	manager       dbus.BusObject
	dnsManager    dbus.BusObject
}

func newNMManager(interfaceName string) (*nmManager, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}

	return &nmManager{
		interfaceName: interfaceName,
		manager:       conn.Object("org.freedesktop.NetworkManager", dbus.ObjectPath("/org/freedesktop/NetworkManager")),
		dnsManager:    conn.Object("org.freedesktop.NetworkManager", dbus.ObjectPath("/org/freedesktop/NetworkManager/DnsManager")),
	}, nil
}

type nmConnectionSettings map[string]map[string]dbus.Variant

func (m *nmManager) SetDNS(config OSConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), reconfigTimeout)
	defer cancel()

	// NetworkManager only lets you set DNS settings on "active"
	// connections, which requires an assigned IP address. This got
	// configured before the DNS manager was invoked, but it might
	// take a little time for the netlink notifications to propagate
	// up. So, keep retrying for the duration of the reconfigTimeout.
	var err error
	for ctx.Err() == nil {
		err = m.trySet(ctx, config)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return err
}

func (m *nmManager) trySet(ctx context.Context, config OSConfig) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("connecting to system bus: %w", err)
	}

	// This is how we get at the DNS settings:
	//
	//               org.freedesktop.NetworkManager
	//                              |
	//                    [GetDeviceByIpIface]
	//                              |
	//                              v
	//           org.freedesktop.NetworkManager.Device <--------\
	//              (describes a network interface)             |
	//                              |                           |
	//                   [GetAppliedConnection]             [Reapply]
	//                              |                           |
	//                              v                           |
	//          org.freedesktop.NetworkManager.Connection       |
	//                   (connection settings)            ------/
	//          contains {dns, dns-priority, dns-search}
	//
	// Ref: https://developer.gnome.org/NetworkManager/stable/settings-ipv4.html.

	nm := conn.Object(
		"org.freedesktop.NetworkManager",
		dbus.ObjectPath("/org/freedesktop/NetworkManager"),
	)

	var devicePath dbus.ObjectPath
	err = nm.CallWithContext(
		ctx, "org.freedesktop.NetworkManager.GetDeviceByIpIface", 0,
		m.interfaceName,
	).Store(&devicePath)
	if err != nil {
		return fmt.Errorf("getDeviceByIpIface: %w", err)
	}
	device := conn.Object("org.freedesktop.NetworkManager", devicePath)

	var (
		settings nmConnectionSettings
		version  uint64
	)
	err = device.CallWithContext(
		ctx, "org.freedesktop.NetworkManager.Device.GetAppliedConnection", 0,
		uint32(0),
	).Store(&settings, &version)
	if err != nil {
		return fmt.Errorf("getAppliedConnection: %w", err)
	}

	// Frustratingly, NetworkManager represents IPv4 addresses as uint32s,
	// although IPv6 addresses are represented as byte arrays.
	// Perform the conversion here.
	var (
		dnsv4 []uint32
		dnsv6 [][]byte
	)
	for _, ip := range config.Nameservers {
		b := ip.As16()
		if ip.Is4() {
			dnsv4 = append(dnsv4, endian.Native.Uint32(b[12:]))
		} else {
			dnsv6 = append(dnsv6, b[:])
		}
	}

	// NetworkManager wipes out IPv6 address configuration unless we
	// tell it explicitly to keep it. Read out the current interface
	// settings and mirror them out to NetworkManager.
	var addrs6 []map[string]any
	if netIface, err := net.InterfaceByName(m.interfaceName); err == nil {
		if addrs, err := netIface.Addrs(); err == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					nip, ok := netip.AddrFromSlice(ipnet.IP)
					nip = nip.Unmap()
					if ok && nip.Is6() {
						addrs6 = append(addrs6, map[string]any{
							"address": nip.String(),
							"prefix":  uint32(128),
						})
					}
				}
			}
		}
	}

	seen := map[dnsname.FQDN]bool{}
	var search []string
	for _, dom := range config.SearchDomains {
		if seen[dom] {
			continue
		}
		seen[dom] = true
		search = append(search, dom.WithTrailingDot())
	}
	for _, dom := range config.MatchDomains {
		if seen[dom] {
			continue
		}
		seen[dom] = true
		search = append(search, "~"+dom.WithTrailingDot())
	}
	if len(config.MatchDomains) == 0 {
		// Non-split routing requested, add an all-domains match.
		search = append(search, "~.")
	}

	// Ideally we would like to disable LLMNR and mdns on the
	// interface here, but older NetworkManagers don't understand
	// those settings and choke on them, so we don't. Both LLMNR and
	// mdns will fail since tailscale0 doesn't do multicast, so it's
	// effectively fine. We used to try and enforce LLMNR and mdns
	// settings here, but that led to #1870.

	ipv4Map := settings["ipv4"]
	ipv4Map["dns"] = dbus.MakeVariant(dnsv4)
	ipv4Map["dns-search"] = dbus.MakeVariant(search)
	// We should only request priority if we have nameservers to set.
	if len(dnsv4) == 0 {
		ipv4Map["dns-priority"] = dbus.MakeVariant(lowerPriority)
	} else if len(config.MatchDomains) > 0 {
		// Set a fairly high priority, but don't override all other
		// configs when in split-DNS mode.
		ipv4Map["dns-priority"] = dbus.MakeVariant(mediumPriority)
	} else {
		// Negative priority means only the settings from the most
		// negative connection get used. The way this mixes with
		// per-domain routing is unclear, but it _seems_ that the
		// priority applies after routing has found possible
		// candidates for a resolution.
		ipv4Map["dns-priority"] = dbus.MakeVariant(highestPriority)
	}

	ipv6Map := settings["ipv6"]
	// In IPv6 settings, you're only allowed to provide additional
	// static DNS settings in "auto" (SLAAC) or "manual" mode. In
	// "manual" mode you also have to specify IP addresses, so we use
	// "auto".
	//
	// NM actually documents that to set just DNS servers, you should
	// use "auto" mode and then set ignore auto routes and DNS, which
	// basically means "autoconfigure but ignore any autoconfiguration
	// results you might get". As a safety, we also say that
	// NetworkManager should never try to make us the default route
	// (none of its business anyway, we handle our own default
	// routing).
	ipv6Map["method"] = dbus.MakeVariant("auto")
	if len(addrs6) > 0 {
		ipv6Map["address-data"] = dbus.MakeVariant(addrs6)
	}
	ipv6Map["ignore-auto-routes"] = dbus.MakeVariant(true)
	ipv6Map["ignore-auto-dns"] = dbus.MakeVariant(true)
	ipv6Map["never-default"] = dbus.MakeVariant(true)

	ipv6Map["dns"] = dbus.MakeVariant(dnsv6)
	ipv6Map["dns-search"] = dbus.MakeVariant(search)
	if len(dnsv6) == 0 {
		ipv6Map["dns-priority"] = dbus.MakeVariant(lowerPriority)
	} else if len(config.MatchDomains) > 0 {
		// Set a fairly high priority, but don't override all other
		// configs when in split-DNS mode.
		ipv6Map["dns-priority"] = dbus.MakeVariant(mediumPriority)
	} else {
		ipv6Map["dns-priority"] = dbus.MakeVariant(highestPriority)
	}

	// deprecatedProperties are the properties in interface settings
	// that are deprecated by NetworkManager.
	//
	// In practice, this means that they are returned for reading,
	// but submitting a settings object with them present fails
	// with hard-to-diagnose errors. They must be removed.
	deprecatedProperties := []string{
		"addresses", "routes",
	}

	for _, property := range deprecatedProperties {
		delete(ipv4Map, property)
		delete(ipv6Map, property)
	}

	if call := device.CallWithContext(ctx, "org.freedesktop.NetworkManager.Device.Reapply", 0, settings, version, uint32(0)); call.Err != nil {
		return fmt.Errorf("reapply: %w", call.Err)
	}

	return nil
}

func (m *nmManager) Close() error {
	// No need to do anything on close, NetworkManager will delete our
	// settings when the tailscale interface goes away.
	return nil
}

func (m *nmManager) Mode() string {
	return "network-maanger"
}
