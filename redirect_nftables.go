//go:build linux

package tun

import (
	"net/netip"

	"github.com/sagernet/nftables"
	"github.com/sagernet/nftables/binaryutil"
	"github.com/sagernet/nftables/expr"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/control"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

func (r *autoRedirect) setupNFTables() error {
	nft, err := nftables.New()
	if err != nil {
		return err
	}
	defer nft.CloseLasting()

	table := nft.AddTable(&nftables.Table{
		Name:   r.tableName,
		Family: nftables.TableFamilyINet,
	})

	err = r.nftablesCreateAddressSets(nft, table, false)
	if err != nil {
		return err
	}

	r.localAddresses = common.FlatMap(r.interfaceFinder.Interfaces(), func(it control.Interface) []netip.Prefix {
		return common.Filter(it.Addresses, func(prefix netip.Prefix) bool {
			return it.Name == "lo" || prefix.Addr().IsGlobalUnicast()
		})
	})
	err = r.nftablesCreateLocalAddressSets(nft, table, r.localAddresses, nil)
	if err != nil {
		return err
	}

	skipOutput := len(r.tunOptions.IncludeInterface) > 0 && !common.Contains(r.tunOptions.IncludeInterface, "lo") || common.Contains(r.tunOptions.ExcludeInterface, "lo")
	if !skipOutput {
		chainOutput := nft.AddChain(&nftables.Chain{
			Name:     "output",
			Table:    table,
			Hooknum:  nftables.ChainHookOutput,
			Priority: nftables.ChainPriorityMangle,
			Type:     nftables.ChainTypeNAT,
		})
		if r.tunOptions.AutoRedirectMarkMode {
			err = r.nftablesCreateExcludeRules(nft, table, chainOutput)
			if err != nil {
				return err
			}
			r.nftablesCreateUnreachable(nft, table, chainOutput)
			r.nftablesCreateRedirect(nft, table, chainOutput)

			chainOutputUDP := nft.AddChain(&nftables.Chain{
				Name:     "output_udp",
				Table:    table,
				Hooknum:  nftables.ChainHookOutput,
				Priority: nftables.ChainPriorityMangle,
				Type:     nftables.ChainTypeRoute,
			})
			err = r.nftablesCreateExcludeRules(nft, table, chainOutputUDP)
			if err != nil {
				return err
			}
			r.nftablesCreateUnreachable(nft, table, chainOutputUDP)
			r.nftablesCreateMark(nft, table, chainOutputUDP)
		} else {
			r.nftablesCreateRedirect(nft, table, chainOutput, &expr.Meta{
				Key:      expr.MetaKeyOIFNAME,
				Register: 1,
			}, &expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     nftablesIfname(r.tunOptions.Name),
			})
		}
	}

	chainPreRouting := nft.AddChain(&nftables.Chain{
		Name:     "prerouting",
		Table:    table,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
		Type:     nftables.ChainTypeNAT,
	})
	err = r.nftablesCreateExcludeRules(nft, table, chainPreRouting)
	if err != nil {
		return err
	}
	r.nftablesCreateUnreachable(nft, table, chainPreRouting)
	r.nftablesCreateRedirect(nft, table, chainPreRouting)
	r.nftablesCreateMark(nft, table, chainPreRouting)

	if r.tunOptions.AutoRedirectMarkMode {
		chainPreRoutingUDP := nft.AddChain(&nftables.Chain{
			Name:     "prerouting_udp",
			Table:    table,
			Hooknum:  nftables.ChainHookPrerouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityMangle + 1),
			Type:     nftables.ChainTypeFilter,
		})
		if r.enableIPv4 {
			nftablesCreateExcludeDestinationIPSet(nft, table, chainPreRoutingUDP, 5, "inet4_local_address_set", nftables.TableFamilyIPv4, false)
		}
		if r.enableIPv6 {
			nftablesCreateExcludeDestinationIPSet(nft, table, chainPreRoutingUDP, 6, "inet6_local_address_set", nftables.TableFamilyIPv6, false)
		}
		nft.AddRule(&nftables.Rule{
			Table: table,
			Chain: chainPreRoutingUDP,
			Exprs: []expr.Any{
				&expr.Meta{
					Key:      expr.MetaKeyL4PROTO,
					Register: 1,
				},
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     []byte{unix.IPPROTO_UDP},
				},
				&expr.Ct{
					Key:      expr.CtKeyMARK,
					Register: 1,
				},
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     binaryutil.NativeEndian.PutUint32(r.tunOptions.AutoRedirectInputMark),
				},
				&expr.Meta{
					Key:            expr.MetaKeyMARK,
					Register:       1,
					SourceRegister: true,
				},
				&expr.Counter{},
			},
		})
	}

	err = r.configureOpenWRTFirewall4(nft, false)
	if err != nil {
		return err
	}

	err = nft.Flush()
	if err != nil {
		return err
	}

	r.networkListener = r.networkMonitor.RegisterCallback(func() {
		err = r.nftablesUpdateLocalAddressSet()
		if err != nil {
			r.logger.Error("update local address set: ", err)
		}
	})
	return nil
}

// TODO; test is this works
func (r *autoRedirect) nftablesUpdateLocalAddressSet() error {
	newLocalAddresses := common.FlatMap(r.interfaceFinder.Interfaces(), func(it control.Interface) []netip.Prefix {
		return common.Filter(it.Addresses, func(prefix netip.Prefix) bool {
			return it.Name == "lo" || prefix.Addr().IsGlobalUnicast()
		})
	})
	if slices.Equal(newLocalAddresses, r.localAddresses) {
		return nil
	}
	nft, err := nftables.New()
	if err != nil {
		return err
	}
	defer nft.CloseLasting()
	table, err := nft.ListTableOfFamily(r.tableName, nftables.TableFamilyINet)
	if err != nil {
		return err
	}
	err = r.nftablesCreateLocalAddressSets(nft, table, newLocalAddresses, r.localAddresses)
	if err != nil {
		return err
	}
	r.localAddresses = newLocalAddresses
	return nft.Flush()
}

func (r *autoRedirect) nftablesUpdateRouteAddressSet() error {
	nft, err := nftables.New()
	if err != nil {
		return err
	}
	defer nft.CloseLasting()
	table, err := nft.ListTableOfFamily(r.tableName, nftables.TableFamilyINet)
	if err != nil {
		return err
	}
	err = r.nftablesCreateAddressSets(nft, table, true)
	if err != nil {
		return err
	}
	return nft.Flush()
}

func (r *autoRedirect) cleanupNFTables() {
	if r.networkListener != nil {
		r.networkMonitor.UnregisterCallback(r.networkListener)
	}
	nft, err := nftables.New()
	if err != nil {
		return
	}
	nft.DelTable(&nftables.Table{
		Name:   r.tableName,
		Family: nftables.TableFamilyINet,
	})
	common.Must(r.configureOpenWRTFirewall4(nft, true))
	_ = nft.Flush()
	_ = nft.CloseLasting()
}