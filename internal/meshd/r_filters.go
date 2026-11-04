package meshd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nft"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
)

// Anycast gateway constants (architecture §7.4). The MAC is the same on
// every node of every mesh; the filters keep it from ever crossing bat0.
const (
	AnycastIfname = "anycast0"
	AnycastMAC    = "02:74:72:65:6c:01"
)

// filtersReconciler owns the bridge-family nftables table on bat0 and the
// client ports (architecture §7.7). Everything that depends on the anycast
// gateway staying local waits for this reconciler to succeed.
type filtersReconciler struct {
	d       *Daemon
	nftPath string
	applied string // script text in force
}

func newFiltersReconciler(d *Daemon) *filtersReconciler { return &filtersReconciler{d: d} }

func (f *filtersReconciler) Name() string { return "filters" }

func (f *filtersReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := f.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var bridgePresent bool
	var apIfname string
	d.facts.Read(func(fa *Facts) { bridgePresent, apIfname = fa.BridgePresent, fa.APIfname })
	set := func(st status.Filters) { d.facts.Update(func(fa *Facts) { fa.Filters = st }) }

	if !bridgePresent {
		set(status.Filters{})
		return res, nil
	}
	if f.nftPath == "" {
		p, err := proc.LookPath("nft")
		if err != nil {
			set(status.Filters{Error: "nft is not installed"})
			warn("error", "filters-no-nft", "nft is not installed, so the bat0 filters cannot be applied and the client network stays off: apt install nftables")
			res.RequeueAfter = 30 * time.Second
			return res, nil
		}
		f.nftPath = p
	}
	ports := append([]string{}, d.clientPorts()...)
	if apIfname != "" {
		ports = append(ports, apIfname)
	}
	anycastIP, err := anycastAddress(d.profile.Prefix)
	if err != nil {
		return res, err
	}
	script := nft.Ruleset{MeshPort: BatIfname, ClientPorts: ports, AnycastIP: anycastIP, AnycastMAC: AnycastMAC, GossipPort: gossip.Port}.Render()

	exists, err := nft.Exists(ctx, f.nftPath)
	if err != nil {
		set(status.Filters{Error: err.Error()})
		warn("error", "filters-failed", "cannot query nftables: %v", err)
		res.RequeueAfter = 30 * time.Second
		return res, nil
	}
	if script != f.applied || !exists {
		if err := nft.Apply(ctx, f.nftPath, script); err != nil {
			f.applied = ""
			set(status.Filters{Error: err.Error()})
			msg := err.Error()
			if strings.Contains(msg, "Operation not supported") || strings.Contains(msg, "No such file") {
				msg += " (does this kernel have CONFIG_NF_TABLES_BRIDGE? see meshctl doctor)"
			}
			warn("error", "filters-failed", "cannot apply the bat0 filters, so the anycast gateway is withheld: %s", msg)
			res.RequeueAfter = 30 * time.Second
			return res, nil
		}
		if f.applied != "" && exists {
			d.log.Info("updated bridge filters", "table", nft.Table, "client_ports", ports)
		} else if exists && script == f.applied {
			d.log.Info("re-applied bridge filters", "table", nft.Table)
		} else {
			if !exists && f.applied != "" {
				d.log.Warn("bridge filters had been removed (nftables.service restart?); re-applied", "table", nft.Table)
			} else {
				d.log.Info("applied bridge filters", "table", nft.Table, "client_ports", ports)
			}
		}
		f.applied = script
		res.Changed = true
	}
	set(status.Filters{Applied: true, Rules: strings.Count(script, "counter")})
	return res, nil
}

func (f *filtersReconciler) Teardown(ctx context.Context) error {
	if f.nftPath == "" {
		return nil
	}
	err := nft.Delete(ctx, f.nftPath)
	if err == nil {
		f.d.log.Info("removed bridge filters", "table", nft.Table)
	}
	f.applied = ""
	return err
}
