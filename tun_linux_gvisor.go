//go:build !no_gvisor && linux

package tun

import (
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

var _ GVisorTun = (*NativeTun)(nil)

func (t *NativeTun) NewEndpoint() (stack.LinkEndpoint, error) {
	return fdbased.New(&fdbased.Options{
		FDs: []int{t.fd},
		MTU: t.mtu,
	})
}