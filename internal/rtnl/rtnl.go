// Package rtnl holds the rtnetlink operations meshd needs that the
// vishvananda/netlink library does not provide directly, plus small helpers
// that make reconcilers read clearly.
package rtnl

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// IFLA_BATADV_ALGO_NAME is batman-adv's rtnetlink link-info attribute for the
// routing algorithm (include/uapi/linux/if_link.h, enum batadv_ifla_attrs).
const IFLA_BATADV_ALGO_NAME = 1

// CreateBatadv creates a batman-adv soft interface with the given routing
// algorithm, in one rtnetlink request. The algorithm is per interface, so
// the module parameter is left alone.
func CreateBatadv(name, algo string, mac net.HardwareAddr) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(name)))
	if len(mac) == 6 {
		req.AddData(nl.NewRtAttr(unix.IFLA_ADDRESS, []byte(mac)))
	}
	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated("batadv"))
	if algo != "" {
		data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
		data.AddRtAttr(IFLA_BATADV_ALGO_NAME, nl.ZeroTerminated(algo))
	}
	req.AddData(linkInfo)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return fmt.Errorf("create batadv link %s (%s): %w", name, algo, err)
	}
	return nil
}

// IsNotFound reports whether err means "no such link".
func IsNotFound(err error) bool {
	var lnf netlink.LinkNotFoundError
	if errors.As(err, &lnf) {
		return true
	}
	return errors.Is(err, unix.ENODEV) || errors.Is(err, os.ErrNotExist)
}

// IsExist reports whether err means "already exists".
func IsExist(err error) bool {
	return errors.Is(err, unix.EEXIST) || errors.Is(err, syscall.EEXIST)
}

// LinkByNameOrNil returns the link, or nil (no error) when it does not exist.
func LinkByNameOrNil(name string) (netlink.Link, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return l, nil
}

// EnsureUp sets the link administratively up if it is not already.
func EnsureUp(l netlink.Link) (changed bool, err error) {
	if l.Attrs().Flags&net.FlagUp != 0 {
		return false, nil
	}
	return true, netlink.LinkSetUp(l)
}

// EnsureMTU sets the MTU if it differs.
func EnsureMTU(l netlink.Link, mtu int) (changed bool, err error) {
	if l.Attrs().MTU == mtu {
		return false, nil
	}
	return true, netlink.LinkSetMTU(l, mtu)
}

// EnsureMaster attaches the link to master (a bridge or a batadv interface)
// if it is not already attached to it.
func EnsureMaster(l netlink.Link, master netlink.Link) (changed bool, err error) {
	if l.Attrs().MasterIndex == master.Attrs().Index {
		return false, nil
	}
	return true, netlink.LinkSetMaster(l, master)
}

// EnsureHardwareAddr sets the MAC if it differs.
func EnsureHardwareAddr(l netlink.Link, mac net.HardwareAddr) (changed bool, err error) {
	if len(mac) != 6 {
		return false, nil
	}
	if l.Attrs().HardwareAddr.String() == mac.String() {
		return false, nil
	}
	return true, netlink.LinkSetHardwareAddr(l, mac)
}

// EnsureAddr adds an IPv4 address (CIDR) to the link unless present.
func EnsureAddr(l netlink.Link, cidr string) (changed bool, err error) {
	want, err := netlink.ParseAddr(cidr)
	if err != nil {
		return false, err
	}
	have, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return false, err
	}
	for _, a := range have {
		if a.IPNet.String() == want.IPNet.String() {
			return false, nil
		}
	}
	return true, netlink.AddrAdd(l, want)
}

// RemoveOtherAddrs deletes IPv4 addresses on the link other than keep (CIDR).
func RemoveOtherAddrs(l netlink.Link, keep string) (removed []string, err error) {
	have, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, a := range have {
		if a.IPNet.String() == keep {
			continue
		}
		if err := netlink.AddrDel(l, &a); err != nil {
			return removed, err
		}
		removed = append(removed, a.IPNet.String())
	}
	return removed, nil
}

// DeleteIfExists deletes a link by name, ignoring "not found".
func DeleteIfExists(name string) (deleted bool, err error) {
	l, err := LinkByNameOrNil(name)
	if err != nil || l == nil {
		return false, err
	}
	if err := netlink.LinkDel(l); err != nil && !IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// LinkEvent is a change to a link, delivered by Subscribe.
type LinkEvent struct {
	Name    string
	Index   int
	Deleted bool
}

// Subscribe delivers link add/delete/change events until done is closed.
func Subscribe(done <-chan struct{}, out chan<- LinkEvent) error {
	ch := make(chan netlink.LinkUpdate, 64)
	if err := netlink.LinkSubscribe(ch, done); err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-done:
				return
			case u, ok := <-ch:
				if !ok {
					return
				}
				ev := LinkEvent{Name: u.Link.Attrs().Name, Index: u.Link.Attrs().Index, Deleted: u.Header.Type == unix.RTM_DELLINK}
				select {
				case out <- ev:
				case <-done:
					return
				}
			}
		}
	}()
	return nil
}

// RuleKey identifies a policy routing rule for comparison.
func RuleKey(r netlink.Rule) string {
	mask := uint32(0)
	if r.Mask != nil {
		mask = *r.Mask
	}
	return fmt.Sprintf("p%d t%d m%#x/%#x i%s o%s", r.Priority, r.Table, r.Mark, mask, r.IifName, r.OifName)
}

// EnsureRules makes the IPv4 rules with priorities in [lo, hi] exactly
// want. It reports whether it changed anything.
func EnsureRules(lo, hi int, want []netlink.Rule) (bool, error) {
	have, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return false, err
	}
	wanted := map[string]netlink.Rule{}
	for _, r := range want {
		wanted[RuleKey(r)] = r
	}
	changed := false
	present := map[string]bool{}
	for _, r := range have {
		if r.Priority < lo || r.Priority > hi {
			continue
		}
		k := RuleKey(r)
		if _, ok := wanted[k]; ok && !present[k] {
			present[k] = true
			continue
		}
		r := r
		if err := netlink.RuleDel(&r); err != nil && !IsNotFound(err) {
			return changed, fmt.Errorf("delete rule %s: %w", k, err)
		}
		changed = true
	}
	for k, r := range wanted {
		if present[k] {
			continue
		}
		r := r
		r.Family = netlink.FAMILY_V4
		if err := netlink.RuleAdd(&r); err != nil && !IsExist(err) {
			return changed, fmt.Errorf("add rule %s: %w", k, err)
		}
		changed = true
	}
	return changed, nil
}

// TableDefault returns the IPv4 default route of a table, or nil.
func TableDefault(table int) (*netlink.Route, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	for i := range routes {
		if routes[i].Dst == nil || routes[i].Dst.IP.IsUnspecified() {
			return &routes[i], nil
		}
	}
	return nil, nil
}

// FlushTables deletes every IPv4 route in tables [lo, hi] except those in
// keep. It reports whether it removed anything.
func FlushTables(lo, hi int, keep map[int]bool) (bool, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, err
	}
	changed := false
	for i := range routes {
		r := routes[i]
		if r.Table < lo || r.Table > hi || keep[r.Table] {
			continue
		}
		if err := netlink.RouteDel(&r); err != nil && !IsNotFound(err) {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// MainDefault returns the main table's lowest-metric IPv4 default route
// that does not leave through one of the excluded interfaces.
func MainDefault(exclude map[string]bool) (*netlink.Route, netlink.Link, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, nil, err
	}
	var best *netlink.Route
	var bestLink netlink.Link
	for i := range routes {
		r := routes[i]
		if r.Dst != nil && !r.Dst.IP.IsUnspecified() {
			continue
		}
		l, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil || exclude[l.Attrs().Name] {
			continue
		}
		if best == nil || r.Priority < best.Priority {
			best, bestLink = &r, l
		}
	}
	return best, bestLink, nil
}
